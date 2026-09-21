package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	"github.com/AGenUI/agenui-studio/harness/sdk"
)

const (
	SchemaVersion          = "agenui.artifact.v1"
	stepMIME               = "application/vnd.agenui.artifact+json"
	defaultMax             = int64(4 << 20)
	envelopeSlack          = int64(4 << 10)
	maxJSONEscapeExpansion = int64(6)
)

var (
	ErrNotFound              = errors.New("AGenUI artifact not found")
	ErrImmutableStepConflict = errors.New("AGenUI artifact store: immutable artifact conflict")
)

const (
	StepSearch             = "search"
	StepCapabilityEvidence = "capability_evidence"
	StepBindingSources     = "binding_sources"
	StepTemplate           = "template"
	StepBinding            = "binding"
	StepContract           = "contract"
	StepEditContract       = "edit_contract"
	StepPreflight          = "preflight"
	StepDesign             = "design"
	StepRequirements       = "requirements"
	StepFinal              = "final"
)

type Pointer struct {
	Ref           string `json:"ref,omitempty"`
	Hash          string `json:"hash,omitempty"`
	MIME          string `json:"mime,omitempty"`
	Size          int64  `json:"size,omitempty"`
	SchemaVersion string `json:"schema_version,omitempty"`
}

// LatestStep describes the newest persisted artifact for a named domain step.
// It is deliberately metadata-only: callers still load the immutable payload
// through Store.Load after choosing the relevant revision.
type LatestStep struct {
	RunID     string
	CreatedAt time.Time
}

type envelope struct {
	SchemaVersion string `json:"schema_version"`
	Kind          string `json:"kind"`
	Payload       string `json:"payload"`
}

// Store is a thin AGenUI adapter over the Harness Artifact Store. Harness owns
// Run lifecycle, durability, ACL and idempotency; Studio owns only typed domain
// payload names and integrity checks.
type Store struct {
	client             harness.ArtifactClient
	maxBytes           int64
	knowledgeAuthority *designknowledge.ReceiptAuthority
}

func NewStore(client harness.ArtifactClient) (*Store, error) {
	if client == nil {
		return nil, errors.New("AGenUI artifact store: client is required")
	}
	return &Store{client: client, maxBytes: defaultMax}, nil
}

func (s *Store) SetDesignKnowledgeAuthority(authority *designknowledge.ReceiptAuthority) {
	if s != nil {
		s.knowledgeAuthority = authority
	}
}

func (s *Store) LatestRunID(ctx context.Context, identity harness.Identity, step string) (string, error) {
	latest, err := s.LatestStep(ctx, identity, step)
	if err != nil {
		return "", err
	}
	return latest.RunID, nil
}

// LatestStep returns the run and creation time for the newest artifact of a
// domain step in a session. Consumers that present an editable Design can use
// this to avoid rendering an older Final artifact after a later Design edit.
func (s *Store) LatestStep(ctx context.Context, identity harness.Identity, step string) (LatestStep, error) {
	if strings.TrimSpace(identity.TenantID) == "" || strings.TrimSpace(identity.UserID) == "" || strings.TrimSpace(identity.SessionID) == "" || !validStep(step) {
		return LatestStep{}, errors.New("AGenUI artifact store: invalid session identity")
	}
	identity.RunID = ""
	page, err := s.client.List(ctx, harness.ListArtifactsRequest{Identity: identity, Kind: harness.ArtifactKindHostData, Limit: 2000})
	if err != nil {
		return LatestStep{}, fmt.Errorf("AGenUI artifact store: list session artifacts: %w", err)
	}
	name := artifactName(step)
	var latest harness.ArtifactInfo
	for _, info := range page.Items {
		if info.Name != name || info.MIME != stepMIME || info.Kind != string(harness.ArtifactKindHostData) || info.ArtifactRef.RunID == "" {
			continue
		}
		if latest.Ref == "" || info.CreatedAt.After(latest.CreatedAt) {
			latest = info
		}
	}
	if latest.Ref == "" {
		return LatestStep{}, ErrNotFound
	}
	return LatestStep{RunID: latest.ArtifactRef.RunID, CreatedAt: latest.CreatedAt}, nil
}

func (s *Store) Save(ctx context.Context, identity harness.Identity, step, content string) (Pointer, error) {
	if err := validateStepIdentity(identity); err != nil {
		return Pointer{}, err
	}
	if !validStep(step) || strings.TrimSpace(content) == "" {
		return Pointer{}, errors.New("AGenUI artifact store: invalid artifact content")
	}
	if int64(len(content)) > s.maxBytes {
		return Pointer{}, fmt.Errorf("AGenUI artifact store: %s exceeds %d bytes", step, s.maxBytes)
	}
	if (step == StepTemplate || step == StepDesign) && s.knowledgeAuthority != nil {
		var design struct {
			Receipt json.RawMessage `json:"design_knowledge_receipt"`
		}
		if err := json.Unmarshal([]byte(content), &design); err != nil || len(design.Receipt) == 0 {
			return Pointer{}, errors.New("AGenUI artifact store: typed design knowledge receipt is missing")
		}
		receipt, err := designknowledge.ParseReceiptJSON(design.Receipt)
		if err != nil {
			return Pointer{}, fmt.Errorf("AGenUI artifact store: design knowledge receipt: %w", err)
		}
		if _, err = s.knowledgeAuthority.Verify(receipt, nil); err != nil {
			return Pointer{}, fmt.Errorf("AGenUI artifact store: design knowledge receipt: %w", err)
		}
	}
	pointers, err := s.runStepPointers(ctx, identity, step)
	if err != nil {
		return Pointer{}, err
	}
	for _, pointer := range pointers {
		stored, loadErr := s.loadPointer(ctx, identity, step, pointer)
		if loadErr != nil {
			return Pointer{}, loadErr
		}
		if stored == content {
			return pointer, nil
		}
	}
	if len(pointers) > 0 {
		return Pointer{}, ErrImmutableStepConflict
	}
	return s.put(ctx, identity, step, artifactName(step), content, artifactIdempotencyKey(identity.RunID, step, content))
}

func (s *Store) Load(ctx context.Context, identity harness.Identity, step string) (string, error) {
	if err := validateStepIdentity(identity); err != nil || !validStep(step) {
		return "", errors.New("AGenUI artifact store: invalid load identity")
	}
	pointers, err := s.runStepPointers(ctx, identity, step)
	if err != nil {
		return "", err
	}
	if len(pointers) == 0 {
		return "", ErrNotFound
	}
	if len(pointers) != 1 {
		return "", errors.New("AGenUI artifact store: artifact has multiple values; use LoadAll")
	}
	return s.loadPointer(ctx, identity, step, pointers[0])
}

func (s *Store) put(ctx context.Context, identity harness.Identity, step, name, content, idempotencyKey string) (Pointer, error) {
	encoded, err := json.Marshal(envelope{SchemaVersion: SchemaVersion, Kind: step, Payload: content})
	if err != nil {
		return Pointer{}, fmt.Errorf("AGenUI artifact store: encode: %w", err)
	}
	if int64(len(encoded)) > s.maxEnvelopeBytes() {
		return Pointer{}, fmt.Errorf("AGenUI artifact store: encoded %s exceeds limit", step)
	}
	info, err := s.client.Put(ctx, harness.PutArtifactRequest{Identity: identity, Name: name, MIME: stepMIME, Kind: harness.ArtifactKindHostData, Content: bytes.NewReader(encoded), IdempotencyKey: idempotencyKey})
	if err != nil {
		return Pointer{}, fmt.Errorf("AGenUI artifact store: put %s: %w", step, err)
	}
	sum := sha256.Sum256(encoded)
	if info.Ref == "" || info.Hash != "sha256:"+hex.EncodeToString(sum[:]) || info.MIME != stepMIME || info.Kind != string(harness.ArtifactKindHostData) || info.SizeBytes != int64(len(encoded)) {
		return Pointer{}, errors.New("AGenUI artifact store: put returned invalid metadata")
	}
	return Pointer{Ref: info.Ref, Hash: info.Hash, MIME: info.MIME, Size: info.SizeBytes, SchemaVersion: SchemaVersion}, nil
}

func (s *Store) LoadAll(ctx context.Context, identity harness.Identity, step string) ([]string, error) {
	if err := validateStepIdentity(identity); err != nil || !validStep(step) {
		return nil, errors.New("AGenUI artifact store: invalid load identity")
	}
	pointers, err := s.runStepPointers(ctx, identity, step)
	if err != nil {
		return nil, err
	}
	if len(pointers) == 0 {
		return nil, ErrNotFound
	}
	sort.Slice(pointers, func(i, j int) bool { return pointers[i].Ref < pointers[j].Ref })
	values := make([]string, 0, len(pointers))
	for _, pointer := range pointers {
		value, loadErr := s.loadPointer(ctx, identity, step, pointer)
		if loadErr != nil {
			return nil, loadErr
		}
		values = append(values, value)
	}
	return values, nil
}

func (s *Store) runStepPointers(ctx context.Context, identity harness.Identity, step string) ([]Pointer, error) {
	page, err := s.client.List(ctx, harness.ListArtifactsRequest{Identity: identity, Kind: harness.ArtifactKindHostData, Limit: 2000})
	if err != nil {
		return nil, fmt.Errorf("AGenUI artifact store: list %s: %w", step, err)
	}
	pointers := make([]Pointer, 0, 1)
	for _, info := range page.Items {
		if !matchesArtifactName(info.Name, step) || info.MIME != stepMIME || info.Kind != string(harness.ArtifactKindHostData) {
			continue
		}
		ref := info.ArtifactRef
		if ref.TenantID != "" && ref.TenantID != identity.TenantID || ref.SessionID != "" && ref.SessionID != identity.SessionID || ref.RunID != "" && ref.RunID != identity.RunID {
			continue
		}
		pointers = append(pointers, Pointer{Ref: info.Ref, Hash: info.Hash, MIME: info.MIME, Size: info.SizeBytes, SchemaVersion: SchemaVersion})
	}
	return pointers, nil
}

func (s *Store) loadPointer(ctx context.Context, identity harness.Identity, step string, pointer Pointer) (string, error) {
	access := harness.Identity{TenantID: identity.TenantID, UserID: identity.UserID, SessionID: identity.SessionID}
	head, err := s.client.Head(ctx, harness.GetArtifactRequest{Identity: access, Ref: pointer.Ref})
	if err != nil {
		return "", fmt.Errorf("AGenUI artifact store: head %s: %w", step, err)
	}
	if head.Ref != pointer.Ref || head.SizeBytes > s.maxEnvelopeBytes() || head.Hash != pointer.Hash || head.MIME != pointer.MIME || head.Kind != string(harness.ArtifactKindHostData) {
		return "", errors.New("AGenUI artifact store: metadata mismatch")
	}
	value, err := s.client.Get(ctx, harness.GetArtifactRequest{Identity: access, Ref: pointer.Ref})
	if err != nil {
		return "", fmt.Errorf("AGenUI artifact store: get %s: %w", step, err)
	}
	if value == nil || value.Content == nil {
		return "", errors.New("AGenUI artifact store: empty content")
	}
	defer value.Content.Close()
	raw, err := io.ReadAll(io.LimitReader(value.Content, s.maxEnvelopeBytes()+1))
	if err != nil {
		return "", fmt.Errorf("AGenUI artifact store: read %s: %w", step, err)
	}
	if int64(len(raw)) > s.maxEnvelopeBytes() {
		return "", errors.New("AGenUI artifact store: content exceeds limit")
	}
	sum := sha256.Sum256(raw)
	if pointer.Hash != "sha256:"+hex.EncodeToString(sum[:]) || pointer.Size != int64(len(raw)) {
		return "", errors.New("AGenUI artifact store: content integrity mismatch")
	}
	var decoded envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("AGenUI artifact store: decode %s: %w", step, err)
	}
	if decoded.SchemaVersion != SchemaVersion || decoded.Kind != step || strings.TrimSpace(decoded.Payload) == "" || int64(len(decoded.Payload)) > s.maxBytes {
		return "", errors.New("AGenUI artifact store: envelope mismatch")
	}
	return decoded.Payload, nil
}

func (s *Store) maxEnvelopeBytes() int64 { return s.maxBytes*maxJSONEscapeExpansion + envelopeSlack }

func artifactName(step string) string { return "agenui-" + step + ".json" }
func matchesArtifactName(name, step string) bool {
	return name == artifactName(step)
}
func artifactIdempotencyKey(runID, step, content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("agenui:%s:%s:%x", runID, step, sum[:])
}
func validateStepIdentity(identity harness.Identity) error {
	if identity.TenantID == "" || identity.UserID == "" || identity.SessionID == "" || identity.RunID == "" {
		return errors.New("AGenUI artifact store: tenant/user/session/run identity is required")
	}
	return nil
}
func validStep(step string) bool {
	switch step {
	case StepSearch, StepCapabilityEvidence, StepBindingSources, StepTemplate, StepBinding, StepContract, StepEditContract, StepPreflight, StepDesign, StepRequirements, StepFinal:
		return true
	default:
		return false
	}
}
func validSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, current := range value[len("sha256:"):] {
		if !(current >= '0' && current <= '9') && !(current >= 'a' && current <= 'f') {
			return false
		}
	}
	return true
}
