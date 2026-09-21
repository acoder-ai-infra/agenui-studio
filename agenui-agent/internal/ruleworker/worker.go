package ruleworker

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui/renderercatalog"
)

const defaultLeaseDuration = 45 * time.Minute

// worker.go is the orchestration: each tick claims pending Markdown documents,
// asks the Harness rule-parser Agent for typed document changes, publishes an
// immutable revision, switches the active pointer, and records the job result.

// Completer runs the agent; *agent.Agent satisfies it. Abstracting it lets tests
// drive the pipeline with a canned response and no network.
type Completer interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// Config parameterises the worker.
type Config struct {
	OutputRoot          string        // where the new revision dir is generated
	OutputPrefix        string        // e.g. "agenui/design-knowledge"
	PollInterval        time.Duration // default 5s
	LeaseDuration       time.Duration // claim lease, default 45m
	WorkerID            string        // identifies this instance in parse_worker
	RendererCatalogRoot string        // selected renderer catalog for rule generation and publication
}

// Worker runs the parse pipeline against local immutable revisions.
type Worker struct {
	store           *Store
	revisionAgent   Completer
	sink            Sink
	baseline        BaselineResolver
	pointer         PointerPublisher
	locker          Locker
	cfg             Config
	rendererCatalog *renderercatalog.Snapshot
}

func (w *Worker) LockAdministrator() (LockAdministrator, bool) {
	admin, ok := w.locker.(LockAdministrator)
	return admin, ok
}

// NewWorker assembles a worker. locker guards "one pass at a time"; pass an
// *InProcessLocker when no distributed lock is needed.
func NewWorker(store *Store, revisionAgent Completer, sink Sink, baseline BaselineResolver, pointer PointerPublisher, locker Locker, cfg Config) *Worker {
	if locker == nil {
		locker = &InProcessLocker{}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = defaultLeaseDuration
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "rule-worker"
	}
	if cfg.OutputPrefix == "" {
		cfg.OutputPrefix = "agenui/design-knowledge"
	}
	worker := &Worker{store: store, revisionAgent: revisionAgent, sink: sink, baseline: baseline, pointer: pointer, locker: locker, cfg: cfg}
	return worker
}

// Run polls until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	log.Printf("[rule-worker] started; worker_id=%s poll=%s lease=%s (baseline resolved from local pointer per pass)",
		w.cfg.WorkerID, w.cfg.PollInterval, w.cfg.LeaseDuration)
	w.logBaselineSnapshot(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Printf("[rule-worker] stopping: %v", ctx.Err())
			return nil
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil {
				log.Printf("[rule-worker] pass error: %v", err)
			}
		}
	}
}

// RunOnce executes one pass. It returns nil when there is nothing to do. A pass
// is guarded by the distributed lock so only one instance runs at a time.
func (w *Worker) RunOnce(ctx context.Context) error {
	// Poll both input tables even when no document is pending. This keeps the
	// operational logs explicit instead of making an idle worker look stuck.
	pending, err := w.store.ListPendingDocs(ctx)
	if err != nil {
		return err
	}
	statusCounts, err := w.store.CountRuleDocsByParseStatus(ctx)
	if err != nil {
		return err
	}
	log.Printf("[rule-worker] poll snapshot table=agenui_rule_doc pending_rows=%d pending_doc_ids=%v parse_status_counts=%s",
		len(pending), docIDs(pending), formatStatusCounts(statusCounts))
	if len(pending) == 0 {
		log.Printf("[rule-worker] poll idle; baseline fetch skipped reason=no_pending_rule_docs")
		return nil
	}
	passStarted := time.Now()
	log.Printf("[rule-worker] pass detected pending_docs=%d doc_ids=%v", len(pending), docIDs(pending))

	lease, acquired, err := w.locker.TryLock(ctx)
	if err != nil {
		return fmt.Errorf("acquire pass lock: %w", err)
	}
	if !acquired {
		log.Printf("[rule-worker] another instance holds the pass lock; skipping this tick")
		return nil
	}
	defer lease.Release()
	passCtx := lease.Context()
	log.Printf("[rule-worker] pass lock acquired elapsed=%s", time.Since(passStarted).Round(time.Millisecond))

	// Re-read under the lock: the previous holder may have just processed these.
	docs, err := w.store.ListPendingDocs(passCtx)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return nil
	}

	// Claim pending and retryable failed documents (0/3 -> 1). Only proceed with
	// the ones we won.
	claimed := make([]RuleDoc, 0, len(docs))
	for _, d := range docs {
		ok, err := w.store.ClaimDoc(passCtx, d.ID, w.cfg.WorkerID, w.cfg.LeaseDuration)
		if err != nil {
			return err
		}
		if ok {
			claimed = append(claimed, d)
		}
	}
	if len(claimed) == 0 {
		return nil
	}
	var docBytes int
	for _, d := range claimed {
		docBytes += len(d.Content)
	}
	log.Printf("[rule-worker] claimed docs=%d doc_ids=%v doc_bytes=%d lease=%s",
		len(claimed), docIDs(claimed), docBytes, w.cfg.LeaseDuration)

	if err := w.process(passCtx, claimed); err != nil {
		// Use an independent bounded context: process cancellation commonly means
		// the service is shutting down or lock ownership was revoked, and the
		// cancelled pass context cannot persist the terminal state.
		markCtx, cancelMark := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelMark()
		if ctx.Err() != nil {
			// Application shutdown is not a parse failure. Return our own claimed
			// documents to pending so the next process can resume them immediately.
			for _, d := range claimed {
				if requeueErr := w.store.RequeueClaimedDoc(markCtx, d.ID, w.cfg.WorkerID); requeueErr != nil {
					log.Printf("[rule-worker] requeue doc %d during shutdown: %v", d.ID, requeueErr)
				}
			}
			log.Printf("[rule-worker] pass interrupted by shutdown; requeued doc_ids=%v elapsed=%s",
				docIDs(claimed), time.Since(passStarted).Round(time.Millisecond))
		} else {
			// Lock revocation and agenuine processing failures remain retryable
			// failures (3), rather than being disguised as never attempted.
			for _, d := range claimed {
				if markErr := w.store.MarkParsed(markCtx, d.ID, 3, err.Error()); markErr != nil {
					log.Printf("[rule-worker] mark doc %d failed: %v", d.ID, markErr)
				}
			}
		}
		log.Printf("[rule-worker] pass failed doc_ids=%v elapsed=%s error=%v",
			docIDs(claimed), time.Since(passStarted).Round(time.Millisecond), err)
		return err
	}
	log.Printf("[rule-worker] pass succeeded doc_ids=%v elapsed=%s",
		docIDs(claimed), time.Since(passStarted).Round(time.Millisecond))
	return nil
}

// logBaselineSnapshot resolves the active baseline once when the worker starts,
// even if both input tables are empty. Normal task passes resolve it again so
// processing always uses the latest local pointer pointer.
func (w *Worker) logBaselineSnapshot(ctx context.Context) {
	started := time.Now()
	log.Printf("[rule-worker] startup baseline snapshot start")
	baseDir, cleanup, err := w.baseline.Resolve(ctx)
	if err != nil {
		log.Printf("[rule-worker] startup baseline snapshot failed elapsed=%s error=%v",
			time.Since(started).Round(time.Millisecond), err)
		return
	}
	if cleanup != nil {
		defer cleanup()
	}
	base, err := LoadBaseRevision(baseDir)
	if err != nil {
		log.Printf("[rule-worker] startup baseline snapshot decode failed elapsed=%s error=%v",
			time.Since(started).Round(time.Millisecond), err)
		return
	}
	_, indexHash, infoErr := revisionInfo(baseDir)
	if infoErr != nil {
		log.Printf("[rule-worker] startup baseline snapshot manifest failed revision=%s elapsed=%s error=%v",
			base.RevisionID, time.Since(started).Round(time.Millisecond), infoErr)
		return
	}
	log.Printf("[rule-worker] startup baseline snapshot ready revision=%s index_hash=%s layouts_bytes=%d foundation_bytes=%d summaries=%d elapsed=%s",
		base.RevisionID, indexHash, len(base.LayoutsMD), len(base.FoundationMD), len(base.DocSummaries),
		time.Since(started).Round(time.Millisecond))
}

func (w *Worker) process(ctx context.Context, docs []RuleDoc) error {
	if strings.TrimSpace(w.cfg.RendererCatalogRoot) != "" {
		snapshot, err := renderercatalog.LoadCurrent(w.cfg.RendererCatalogRoot)
		if err != nil {
			return fmt.Errorf("load renderer catalog: %w", err)
		}
		w.rendererCatalog = &snapshot
	}
	// Resolve the baseline revision from local pointer (pointer -> local artifact/local zip).
	baselineStarted := time.Now()
	log.Printf("[rule-worker] baseline resolve start doc_ids=%v", docIDs(docs))
	baseDir, cleanup, err := w.baseline.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("resolve baseline: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	base, err := LoadBaseRevision(baseDir)
	if err != nil {
		return err
	}
	log.Printf("[rule-worker] baseline ready revision=%s layouts_bytes=%d foundation_bytes=%d summaries=%d elapsed=%s",
		base.RevisionID, len(base.LayoutsMD), len(base.FoundationMD), len(base.DocSummaries),
		time.Since(baselineStarted).Round(time.Millisecond))

	// The model only plans the next immutable design-knowledge revision.
	user := BuildRevisionUserPrompt(base, docs)
	if w.rendererCatalog != nil {
		catalogJSON, err := w.rendererCatalog.AuthoringJSON()
		if err != nil {
			return err
		}
		user += "\n\n# 当前 Renderer Catalog（唯一组件与属性协议）\n下面是从当前发布且校验过 catalog 动态提取的 authoring view，保留全部组件和属性定义；它不是另一份能力配置。规则设计只能使用其中定义的组件与属性；不能从自然语言关键词猜测或补造能力。\n```json\n" + string(catalogJSON) + "\n```"
	}
	agentStarted := time.Now()
	log.Printf("[rule-worker] revision agent start revision=%s doc_ids=%v system_bytes=%d user_bytes=%d",
		base.RevisionID, docIDs(docs), len(revisionSystemPrompt), len(user))
	rawJSON, err := w.revisionAgent.Complete(ctx, revisionSystemPrompt, user)
	if err != nil {
		log.Printf("[rule-worker] revision agent failed revision=%s doc_ids=%v elapsed=%s error=%v",
			base.RevisionID, docIDs(docs), time.Since(agentStarted).Round(time.Millisecond), err)
		return fmt.Errorf("revision agent completion: %w", err)
	}
	log.Printf("[rule-worker] revision agent succeeded revision=%s response_bytes=%d elapsed=%s",
		base.RevisionID, len(rawJSON), time.Since(agentStarted).Round(time.Millisecond))
	var plan RevisionPlan
	if err := json.Unmarshal([]byte(rawJSON), &plan); err != nil {
		return fmt.Errorf("decode revision plan: %w", err)
	}
	if err := MaterializeDocumentContracts(&plan.Delta, true); err != nil {
		return fmt.Errorf("materialize structured revision documents: %w", err)
	}

	// Materialize and self-check the latest revision before publication.
	newRevID, err := NextRevisionID(base.RevisionID)
	if err != nil {
		return err
	}
	outDir := filepath.Join(w.cfg.OutputRoot, newRevID)
	assignedLayouts, err := GenerateRevision(ctx, baseDir, outDir, newRevID, plan.Delta)
	if err != nil {
		return fmt.Errorf("generate revision %s: %w", newRevID, err)
	}
	log.Printf("[rule-worker] generated revision %s (new layouts: %v)", newRevID, assignedLayouts)

	zipLoc, err := w.uploadOutputs(ctx, newRevID, outDir)
	if err != nil {
		return fmt.Errorf("upload outputs: %w", err)
	}

	// Publish the new pointer to local pointer so the baseline advances to this
	// revision (local: file path; other envs: local artifact:// URL — whatever the sink
	// returned). The index hash comes from the freshly published manifest.
	_, indexHash, err := revisionInfo(outDir)
	if err != nil {
		return fmt.Errorf("read revision info: %w", err)
	}
	if err := w.pointer.Publish(ctx, zipLoc, newRevID, indexHash); err != nil {
		return fmt.Errorf("publish local pointer pointer: %w", err)
	}
	log.Printf("[rule-worker] published local pointer pointer -> %s (revision=%s)", zipLoc, newRevID)

	if err := w.store.PublishDocs(ctx, docs); err != nil {
		return fmt.Errorf("record published rule documents: %w", err)
	}
	log.Printf("[rule-worker] pass complete: revision=%s docs=%d", newRevID, len(docs))
	return nil
}

// uploadOutputs publishes the immutable revision directory as one archive.
func (w *Worker) uploadOutputs(ctx context.Context, revision, outDir string) (string, error) {
	prefix := path.Join(w.cfg.OutputPrefix, revision)

	// The entire final revision directory as one zip (entries rooted at the
	// revision id, so unzip yields <revision>/...).
	archive, err := zipDir(outDir, revision)
	if err != nil {
		return "", fmt.Errorf("zip revision: %w", err)
	}
	loc, err := w.sink.Put(ctx, path.Join(prefix, revision+".zip"), archive)
	if err != nil {
		return "", err
	}
	log.Printf("[rule-worker] uploaded revision zip (%d bytes) -> %s", len(archive), loc)
	return loc, nil
}

// zipDir builds an in-memory zip of every file under dir, with entry paths
// prefixed by rootName (e.g. "revision-5/layouts/summary-card.md"). Entries are written in a
// deterministic (sorted) order.
func zipDir(dir, rootName string) ([]byte, error) {
	var files []string
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(files)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range files {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil, err
		}
		entry, err := zw.Create(path.Join(rootName, filepath.ToSlash(rel)))
		if err != nil {
			return nil, err
		}
		src, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(entry, src); err != nil {
			_ = src.Close()
			return nil, err
		}
		_ = src.Close()
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func docIDs(docs []RuleDoc) []int64 {
	ids := make([]int64, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func formatStatusCounts(counts []RuleDocStatusCount) string {
	if len(counts) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(counts))
	for _, item := range counts {
		parts = append(parts, fmt.Sprintf("%d:%d", item.ParseStatus, item.Count))
	}
	return "[" + strings.Join(parts, ",") + "]"
}
