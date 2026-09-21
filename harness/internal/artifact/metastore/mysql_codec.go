package metastore

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

const mysqlCodecVersion = 1

var errMySQLNonCanonicalBase64 = errors.New("non-canonical base64")

type mysqlPreviewPayload struct {
	Version   int            `json:"version"`
	Text      string         `json:"text"`
	Fields    mysqlTypedNode `json:"fields"`
	Truncated bool           `json:"truncated"`
}

type mysqlTypedNode struct {
	Kind    string             `json:"kind"`
	Nil     bool               `json:"nil"`
	String  *string            `json:"string,omitempty"`
	Bool    *bool              `json:"bool,omitempty"`
	Float   *string            `json:"float_bits,omitempty"`
	Int     *string            `json:"int,omitempty"`
	Strings *[]string          `json:"strings,omitempty"`
	Items   *[]mysqlTypedNode  `json:"items,omitempty"`
	Entries *[]mysqlTypedEntry `json:"entries,omitempty"`
}

type mysqlTypedEntry struct {
	Key   string         `json:"key"`
	Value mysqlTypedNode `json:"value"`
}

type mysqlLineagePayload struct {
	Version int                `json:"version"`
	Items   []mysqlLineageItem `json:"items"`
}

type mysqlLineageItem struct {
	ArtifactRef string `json:"artifact_ref"`
	Relation    string `json:"relation"`
}

type mysqlMetadataPayload struct {
	Version int                  `json:"version"`
	Entries []mysqlMetadataEntry `json:"entries"`
}

type mysqlMetadataEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type mysqlTimeColumns struct {
	Seconds     sql.NullInt64
	Nanoseconds sql.NullInt64
}

// mysqlArtifactRow is the codec-owned projection of artifact_metadata. Digest,
// record ID, and idempotency columns are transaction/query concerns and are
// intentionally kept out of this pre-transaction representation.
type mysqlArtifactRow struct {
	artifactID         []byte
	artifactRef        []byte
	tenantID           []byte
	userID             []byte
	sessionID          []byte
	runID              []byte
	stepID             []byte
	ownerModule        []byte
	ownerID            []byte
	artifactType       []byte
	mimeType           []byte
	name               []byte
	sizeBytes          int64
	artifactHash       []byte
	visibility         []byte
	storageBackend     []byte
	storageKey         []byte
	previewPayload     []byte
	retentionPolicy    []byte
	expiresAt          mysqlTimeColumns
	createdBy          []byte
	createdAt          mysqlTimeColumns
	status             []byte
	derivedFromPayload []byte
	schemaVersion      []byte
	deletedAt          mysqlTimeColumns
	deleteReason       []byte
	purgeStatus        []byte
	purgedAt           mysqlTimeColumns
	metadataPayload    []byte
}

func encodeMySQLPreview(preview artifact.Preview) ([]byte, error) {
	fields, err := encodeMySQLPreviewValue(preview.Fields)
	if err != nil {
		return nil, err
	}
	return json.Marshal(mysqlPreviewPayload{
		Version:   mysqlCodecVersion,
		Text:      encodeMySQLString(preview.Text),
		Fields:    fields,
		Truncated: preview.Truncated,
	})
}

func decodeMySQLPreview(payload []byte) (artifact.Preview, error) {
	wire, err := parseMySQLJSONWire(payload, "preview")
	if err != nil {
		return artifact.Preview{}, err
	}
	if err := validateMySQLPreviewWire(wire); err != nil {
		return artifact.Preview{}, err
	}
	var encoded mysqlPreviewPayload
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&encoded); err != nil {
		return artifact.Preview{}, mysqlCodecInvalidCause("malformed MySQL preview payload", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return artifact.Preview{}, mysqlCodecInvalid("trailing JSON in MySQL preview payload")
		}
		return artifact.Preview{}, mysqlCodecInvalidCause("invalid trailing data in MySQL preview payload", err)
	}
	if encoded.Version != mysqlCodecVersion {
		return artifact.Preview{}, mysqlCodecInvalid("unknown MySQL preview codec version")
	}
	fields, err := decodeMySQLPreviewValue(encoded.Fields)
	if err != nil {
		return artifact.Preview{}, err
	}
	typedFields, ok := fields.(map[string]any)
	if !ok {
		return artifact.Preview{}, mysqlCodecInvalid("MySQL preview fields payload is not a map")
	}
	text, err := decodeMySQLString(encoded.Text)
	if err != nil {
		return artifact.Preview{}, err
	}
	return artifact.Preview{Text: text, Fields: typedFields, Truncated: encoded.Truncated}, nil
}

func encodeMySQLLineage(lineage []artifact.ArtifactLineage) ([]byte, error) {
	if lineage == nil {
		return nil, nil
	}
	items := make([]mysqlLineageItem, len(lineage))
	for index, item := range lineage {
		items[index] = mysqlLineageItem{
			ArtifactRef: encodeMySQLString(item.ArtifactRef),
			Relation:    encodeMySQLString(string(item.Relation)),
		}
	}
	return json.Marshal(mysqlLineagePayload{Version: mysqlCodecVersion, Items: items})
}

func decodeMySQLLineage(payload []byte) ([]artifact.ArtifactLineage, error) {
	if payload == nil {
		return nil, nil
	}
	wire, err := parseMySQLJSONWire(payload, "lineage")
	if err != nil {
		return nil, err
	}
	if err := validateMySQLLineageWire(wire); err != nil {
		return nil, err
	}
	var encoded mysqlLineagePayload
	if err := decodeMySQLJSONPayload(payload, &encoded, "lineage"); err != nil {
		return nil, err
	}
	if encoded.Version != mysqlCodecVersion {
		return nil, mysqlCodecInvalid("unknown MySQL lineage codec version")
	}
	if encoded.Items == nil {
		return nil, mysqlCodecInvalid("malformed MySQL lineage payload")
	}
	lineage := make([]artifact.ArtifactLineage, len(encoded.Items))
	for index, item := range encoded.Items {
		ref, err := decodeMySQLString(item.ArtifactRef)
		if err != nil {
			return nil, err
		}
		relation, err := decodeMySQLString(item.Relation)
		if err != nil {
			return nil, err
		}
		lineage[index] = artifact.ArtifactLineage{ArtifactRef: ref, Relation: artifact.LineageRelation(relation)}
	}
	return lineage, nil
}

func encodeMySQLMetadata(metadata map[string]string) ([]byte, error) {
	if metadata == nil {
		return nil, nil
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]mysqlMetadataEntry, len(keys))
	for index, key := range keys {
		entries[index] = mysqlMetadataEntry{
			Key:   encodeMySQLString(key),
			Value: encodeMySQLString(metadata[key]),
		}
	}
	return json.Marshal(mysqlMetadataPayload{Version: mysqlCodecVersion, Entries: entries})
}

func decodeMySQLMetadata(payload []byte) (map[string]string, error) {
	if payload == nil {
		return nil, nil
	}
	wire, err := parseMySQLJSONWire(payload, "metadata")
	if err != nil {
		return nil, err
	}
	if err := validateMySQLMetadataWire(wire); err != nil {
		return nil, err
	}
	var encoded mysqlMetadataPayload
	if err := decodeMySQLJSONPayload(payload, &encoded, "metadata"); err != nil {
		return nil, err
	}
	if encoded.Version != mysqlCodecVersion {
		return nil, mysqlCodecInvalid("unknown MySQL metadata codec version")
	}
	if encoded.Entries == nil {
		return nil, mysqlCodecInvalid("malformed MySQL metadata payload")
	}
	metadata := make(map[string]string, len(encoded.Entries))
	seen := make(map[string]struct{}, len(encoded.Entries))
	for _, entry := range encoded.Entries {
		key, err := decodeMySQLString(entry.Key)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, mysqlCodecInvalid("duplicate key in MySQL metadata payload")
		}
		seen[key] = struct{}{}
		value, err := decodeMySQLString(entry.Value)
		if err != nil {
			return nil, err
		}
		metadata[key] = value
	}
	return metadata, nil
}

func encodeNullableMySQLTime(value time.Time) mysqlTimeColumns {
	if value.IsZero() {
		return mysqlTimeColumns{}
	}
	return encodeRequiredMySQLTime(value)
}

func encodeRequiredMySQLTime(value time.Time) mysqlTimeColumns {
	return mysqlTimeColumns{
		Seconds:     sql.NullInt64{Int64: value.Unix(), Valid: true},
		Nanoseconds: sql.NullInt64{Int64: int64(value.Nanosecond()), Valid: true},
	}
}

func decodeNullableMySQLTime(columns mysqlTimeColumns) (time.Time, error) {
	if columns.Seconds.Valid != columns.Nanoseconds.Valid {
		return time.Time{}, mysqlCodecInvalid("mismatched nullable MySQL time columns")
	}
	if !columns.Seconds.Valid {
		return time.Time{}, nil
	}
	return decodePopulatedMySQLTime(columns)
}

func decodeRequiredMySQLTime(columns mysqlTimeColumns) (time.Time, error) {
	if !columns.Seconds.Valid || !columns.Nanoseconds.Valid {
		return time.Time{}, mysqlCodecInvalid("required MySQL time columns are NULL")
	}
	return decodePopulatedMySQLTime(columns)
}

func decodePopulatedMySQLTime(columns mysqlTimeColumns) (time.Time, error) {
	if columns.Nanoseconds.Int64 < 0 || columns.Nanoseconds.Int64 >= int64(time.Second) {
		return time.Time{}, mysqlCodecInvalid("invalid nanoseconds in MySQL time columns")
	}
	return time.Unix(columns.Seconds.Int64, columns.Nanoseconds.Int64).UTC(), nil
}

// prepareMySQLArtifactRow performs all fallible portable encoding before a
// caller opens a write transaction. The second result is the immediate Create
// result: it owns every container while retaining the input time.Time values,
// including their monotonic readings and location representation.
func prepareMySQLArtifactRow(meta artifact.ArtifactMeta) (mysqlArtifactRow, *artifact.ArtifactMeta, error) {
	previewPayload, err := encodeMySQLPreview(meta.Preview)
	if err != nil {
		return mysqlArtifactRow{}, nil, err
	}
	lineagePayload, err := encodeMySQLLineage(meta.DerivedFrom)
	if err != nil {
		return mysqlArtifactRow{}, nil, err
	}
	metadataPayload, err := encodeMySQLMetadata(meta.Metadata)
	if err != nil {
		return mysqlArtifactRow{}, nil, err
	}

	previewClone, err := decodeMySQLPreview(previewPayload)
	if err != nil {
		return mysqlArtifactRow{}, nil, err
	}
	lineageClone, err := decodeMySQLLineage(lineagePayload)
	if err != nil {
		return mysqlArtifactRow{}, nil, err
	}
	metadataClone, err := decodeMySQLMetadata(metadataPayload)
	if err != nil {
		return mysqlArtifactRow{}, nil, err
	}
	immediate := meta
	immediate.Preview = previewClone
	immediate.DerivedFrom = lineageClone
	immediate.Metadata = metadataClone

	row := mysqlArtifactRow{
		artifactID:         []byte(meta.ArtifactID),
		artifactRef:        []byte(meta.ArtifactRef),
		tenantID:           []byte(meta.TenantID),
		userID:             []byte(meta.UserID),
		sessionID:          []byte(meta.SessionID),
		runID:              []byte(meta.RunID),
		stepID:             []byte(meta.StepID),
		ownerModule:        []byte(meta.OwnerModule),
		ownerID:            []byte(meta.OwnerID),
		artifactType:       []byte(meta.ArtifactType),
		mimeType:           []byte(meta.MimeType),
		name:               []byte(meta.Name),
		sizeBytes:          meta.SizeBytes,
		artifactHash:       []byte(meta.Hash),
		visibility:         []byte(meta.Visibility),
		storageBackend:     []byte(meta.StorageBackend),
		storageKey:         []byte(meta.StorageKey),
		previewPayload:     previewPayload,
		retentionPolicy:    []byte(meta.RetentionPolicy),
		expiresAt:          encodeNullableMySQLTime(meta.ExpiresAt),
		createdBy:          []byte(meta.CreatedBy),
		createdAt:          encodeRequiredMySQLTime(meta.CreatedAt),
		status:             []byte(meta.Status),
		derivedFromPayload: lineagePayload,
		schemaVersion:      []byte(meta.SchemaVersion),
		deletedAt:          encodeNullableMySQLTime(meta.DeletedAt),
		deleteReason:       []byte(meta.DeleteReason),
		purgeStatus:        []byte(meta.PurgeStatus),
		purgedAt:           encodeNullableMySQLTime(meta.PurgedAt),
		metadataPayload:    metadataPayload,
	}
	return row, &immediate, nil
}

func decodeMySQLArtifactRow(row mysqlArtifactRow) (*artifact.ArtifactMeta, error) {
	preview, err := decodeMySQLPreview(row.previewPayload)
	if err != nil {
		return nil, err
	}
	lineage, err := decodeMySQLLineage(row.derivedFromPayload)
	if err != nil {
		return nil, err
	}
	metadata, err := decodeMySQLMetadata(row.metadataPayload)
	if err != nil {
		return nil, err
	}
	expiresAt, err := decodeNullableMySQLTime(row.expiresAt)
	if err != nil {
		return nil, err
	}
	createdAt, err := decodeRequiredMySQLTime(row.createdAt)
	if err != nil {
		return nil, err
	}
	deletedAt, err := decodeNullableMySQLTime(row.deletedAt)
	if err != nil {
		return nil, err
	}
	purgedAt, err := decodeNullableMySQLTime(row.purgedAt)
	if err != nil {
		return nil, err
	}
	return &artifact.ArtifactMeta{
		ArtifactID:      string(row.artifactID),
		ArtifactRef:     string(row.artifactRef),
		TenantID:        string(row.tenantID),
		UserID:          string(row.userID),
		SessionID:       string(row.sessionID),
		RunID:           string(row.runID),
		StepID:          string(row.stepID),
		OwnerModule:     artifact.OwnerModule(row.ownerModule),
		OwnerID:         string(row.ownerID),
		ArtifactType:    artifact.ArtifactType(row.artifactType),
		MimeType:        string(row.mimeType),
		Name:            string(row.name),
		SizeBytes:       row.sizeBytes,
		Hash:            string(row.artifactHash),
		Visibility:      artifact.Visibility(row.visibility),
		StorageBackend:  string(row.storageBackend),
		StorageKey:      string(row.storageKey),
		Preview:         preview,
		RetentionPolicy: artifact.RetentionPolicy(row.retentionPolicy),
		ExpiresAt:       expiresAt,
		CreatedBy:       string(row.createdBy),
		CreatedAt:       createdAt,
		Status:          artifact.ArtifactStatus(row.status),
		DerivedFrom:     lineage,
		SchemaVersion:   string(row.schemaVersion),
		DeletedAt:       deletedAt,
		DeleteReason:    artifact.DeleteReason(row.deleteReason),
		PurgeStatus:     artifact.PurgeStatus(row.purgeStatus),
		PurgedAt:        purgedAt,
		Metadata:        metadata,
	}, nil
}

func encodeMySQLPreviewValue(value any) (mysqlTypedNode, error) {
	state := mysqlPreviewEncodingState{
		activeMaps:   make(map[uintptr]struct{}),
		activeSlices: make(map[mysqlPreviewSliceIdentity]struct{}),
	}
	return encodeMySQLPreviewValueWithState(value, &state)
}

type mysqlPreviewEncodingState struct {
	activeMaps   map[uintptr]struct{}
	activeSlices map[mysqlPreviewSliceIdentity]struct{}
}

type mysqlPreviewSliceIdentity struct {
	pointer uintptr
	length  int
}

func encodeMySQLPreviewValueWithState(value any, state *mysqlPreviewEncodingState) (mysqlTypedNode, error) {
	switch typed := value.(type) {
	case nil:
		return mysqlTypedNode{Kind: "nil", Nil: true}, nil
	case string:
		encoded := encodeMySQLString(typed)
		return mysqlTypedNode{Kind: "string", String: &encoded}, nil
	case bool:
		return mysqlTypedNode{Kind: "bool", Bool: &typed}, nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return mysqlTypedNode{}, mysqlCodecInvalid("non-finite preview float64 values are not portable")
		}
		bits := fmt.Sprintf("%016x", math.Float64bits(typed))
		return mysqlTypedNode{Kind: "float64", Float: &bits}, nil
	case int64:
		value := strconv.FormatInt(typed, 10)
		return mysqlTypedNode{Kind: "int64", Int: &value}, nil
	case []string:
		if typed == nil {
			return mysqlTypedNode{Kind: "strings", Nil: true}, nil
		}
		values := make([]string, len(typed))
		for index, value := range typed {
			values[index] = encodeMySQLString(value)
		}
		return mysqlTypedNode{Kind: "strings", Strings: &values}, nil
	case []any:
		if typed == nil {
			return mysqlTypedNode{Kind: "items", Nil: true}, nil
		}
		if len(typed) > 0 {
			identity := mysqlPreviewSliceIdentity{
				pointer: reflect.ValueOf(typed).Pointer(),
				length:  len(typed),
			}
			if _, active := state.activeSlices[identity]; active {
				return mysqlTypedNode{}, mysqlCodecInvalid("cyclic preview slices are not portable")
			}
			state.activeSlices[identity] = struct{}{}
			defer delete(state.activeSlices, identity)
		}
		items := make([]mysqlTypedNode, len(typed))
		for index, item := range typed {
			encoded, err := encodeMySQLPreviewValueWithState(item, state)
			if err != nil {
				return mysqlTypedNode{}, err
			}
			items[index] = encoded
		}
		return mysqlTypedNode{Kind: "items", Items: &items}, nil
	case map[string]any:
		if typed == nil {
			return mysqlTypedNode{Kind: "map", Nil: true}, nil
		}
		identity := reflect.ValueOf(typed).Pointer()
		if _, active := state.activeMaps[identity]; active {
			return mysqlTypedNode{}, mysqlCodecInvalid("cyclic preview maps are not portable")
		}
		state.activeMaps[identity] = struct{}{}
		defer delete(state.activeMaps, identity)
		entries := make([]mysqlTypedEntry, 0, len(typed))
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item := typed[key]
			encoded, err := encodeMySQLPreviewValueWithState(item, state)
			if err != nil {
				return mysqlTypedNode{}, err
			}
			entries = append(entries, mysqlTypedEntry{Key: encodeMySQLString(key), Value: encoded})
		}
		return mysqlTypedNode{Kind: "map", Entries: &entries}, nil
	case int:
		return mysqlTypedNode{}, mysqlCodecInvalid("preview int values are not portable")
	case []int:
		return mysqlTypedNode{}, mysqlCodecInvalid("preview []int values are not portable")
	case []byte:
		return mysqlTypedNode{}, mysqlCodecInvalid("preview []byte values are not portable")
	case map[string]string:
		return mysqlTypedNode{}, mysqlCodecInvalid("preview map[string]string values are not portable")
	default:
		if typeOf := reflect.TypeOf(value); typeOf != nil {
			if typeOf.Name() != "" {
				return mysqlTypedNode{}, mysqlCodecInvalid("named preview values are not portable")
			}
			if typeOf.Kind() == reflect.Func || typeOf.Kind() == reflect.Chan {
				return mysqlTypedNode{}, mysqlCodecInvalid("function and channel preview values are not portable")
			}
		}
		return mysqlTypedNode{}, mysqlCodecInvalid("unsupported concrete type in MySQL preview")
	}
}

func decodeMySQLPreviewValue(node mysqlTypedNode) (any, error) {
	if err := validateMySQLTypedNode(node); err != nil {
		return nil, err
	}
	switch node.Kind {
	case "nil":
		return nil, nil
	case "string":
		return decodeMySQLString(*node.String)
	case "bool":
		return *node.Bool, nil
	case "float64":
		if len(*node.Float) != 16 {
			return nil, mysqlCodecInvalid("invalid float64 bit width in MySQL preview payload")
		}
		bits, err := strconv.ParseUint(*node.Float, 16, 64)
		if err != nil {
			return nil, mysqlCodecInvalidCause("invalid float64 bits in MySQL preview payload", err)
		}
		value := math.Float64frombits(bits)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, mysqlCodecInvalid("non-finite float64 in MySQL preview payload")
		}
		return value, nil
	case "int64":
		value, err := strconv.ParseInt(*node.Int, 10, 64)
		if err != nil {
			return nil, mysqlCodecInvalidCause("invalid int64 in MySQL preview payload", err)
		}
		return value, nil
	case "strings":
		if node.Nil {
			return []string(nil), nil
		}
		values := make([]string, len(*node.Strings))
		for index, value := range *node.Strings {
			decoded, err := decodeMySQLString(value)
			if err != nil {
				return nil, err
			}
			values[index] = decoded
		}
		return values, nil
	case "items":
		if node.Nil {
			return []any(nil), nil
		}
		items := make([]any, len(*node.Items))
		for index, item := range *node.Items {
			decoded, err := decodeMySQLPreviewValue(item)
			if err != nil {
				return nil, err
			}
			items[index] = decoded
		}
		return items, nil
	case "map":
		if node.Nil {
			return map[string]any(nil), nil
		}
		decoded := make(map[string]any, len(*node.Entries))
		seen := make(map[string]struct{}, len(*node.Entries))
		for _, entry := range *node.Entries {
			key, err := decodeMySQLString(entry.Key)
			if err != nil {
				return nil, err
			}
			if _, duplicate := seen[key]; duplicate {
				return nil, mysqlCodecInvalid("duplicate map key in MySQL preview payload")
			}
			seen[key] = struct{}{}
			value, err := decodeMySQLPreviewValue(entry.Value)
			if err != nil {
				return nil, err
			}
			decoded[key] = value
		}
		return decoded, nil
	default:
		return nil, mysqlCodecInvalid("unknown MySQL preview value kind")
	}
}

func validateMySQLTypedNode(node mysqlTypedNode) error {
	noValues := func() bool {
		return node.String == nil && node.Bool == nil && node.Float == nil && node.Int == nil &&
			node.Strings == nil && node.Items == nil && node.Entries == nil
	}
	scalarOnly := func(valuePresent bool) bool {
		present := 0
		for _, exists := range []bool{
			node.String != nil,
			node.Bool != nil,
			node.Float != nil,
			node.Int != nil,
			node.Strings != nil,
			node.Items != nil,
			node.Entries != nil,
		} {
			if exists {
				present++
			}
		}
		return valuePresent && present == 1
	}

	valid := false
	switch node.Kind {
	case "nil":
		valid = node.Nil && noValues()
	case "string":
		valid = !node.Nil && scalarOnly(node.String != nil)
	case "bool":
		valid = !node.Nil && scalarOnly(node.Bool != nil)
	case "float64":
		valid = !node.Nil && scalarOnly(node.Float != nil)
	case "int64":
		valid = !node.Nil && scalarOnly(node.Int != nil)
	case "strings":
		valid = (node.Nil && noValues()) || (!node.Nil && scalarOnly(node.Strings != nil))
	case "items":
		valid = (node.Nil && noValues()) || (!node.Nil && scalarOnly(node.Items != nil))
	case "map":
		valid = (node.Nil && noValues()) || (!node.Nil && scalarOnly(node.Entries != nil))
	default:
		return mysqlCodecInvalid("unknown MySQL preview value kind")
	}
	if !valid {
		return mysqlCodecInvalid("malformed typed node in MySQL preview payload")
	}
	return nil
}

type mysqlJSONWireKind uint8

const (
	mysqlJSONWireNull mysqlJSONWireKind = iota
	mysqlJSONWireObject
	mysqlJSONWireArray
	mysqlJSONWireString
	mysqlJSONWireBool
	mysqlJSONWireNumber
)

type mysqlJSONWireValue struct {
	kind        mysqlJSONWireKind
	object      []mysqlJSONWireMember
	array       []mysqlJSONWireValue
	stringValue string
	boolValue   bool
}

type mysqlJSONWireMember struct {
	name  string
	value mysqlJSONWireValue
}

func parseMySQLJSONWire(payload []byte, name string) (mysqlJSONWireValue, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	value, err := readMySQLJSONWireValue(decoder)
	if err != nil {
		return mysqlJSONWireValue{}, mysqlCodecInvalidCause("malformed MySQL "+name+" payload", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return mysqlJSONWireValue{}, mysqlCodecInvalid("trailing JSON in MySQL " + name + " payload")
		}
		return mysqlJSONWireValue{}, mysqlCodecInvalidCause("invalid trailing data in MySQL "+name+" payload", err)
	}
	return value, nil
}

func readMySQLJSONWireValue(decoder *json.Decoder) (mysqlJSONWireValue, error) {
	token, err := decoder.Token()
	if err != nil {
		return mysqlJSONWireValue{}, err
	}
	switch typed := token.(type) {
	case nil:
		return mysqlJSONWireValue{kind: mysqlJSONWireNull}, nil
	case string:
		return mysqlJSONWireValue{kind: mysqlJSONWireString, stringValue: typed}, nil
	case bool:
		return mysqlJSONWireValue{kind: mysqlJSONWireBool, boolValue: typed}, nil
	case json.Number:
		return mysqlJSONWireValue{kind: mysqlJSONWireNumber}, nil
	case json.Delim:
		switch typed {
		case '{':
			members := make([]mysqlJSONWireMember, 0)
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return mysqlJSONWireValue{}, err
				}
				memberName, ok := nameToken.(string)
				if !ok {
					return mysqlJSONWireValue{}, fmt.Errorf("JSON object member name has type %T", nameToken)
				}
				memberValue, err := readMySQLJSONWireValue(decoder)
				if err != nil {
					return mysqlJSONWireValue{}, err
				}
				members = append(members, mysqlJSONWireMember{name: memberName, value: memberValue})
			}
			if err := readMySQLJSONWireClosingDelimiter(decoder, '}'); err != nil {
				return mysqlJSONWireValue{}, err
			}
			return mysqlJSONWireValue{kind: mysqlJSONWireObject, object: members}, nil
		case '[':
			items := make([]mysqlJSONWireValue, 0)
			for decoder.More() {
				item, err := readMySQLJSONWireValue(decoder)
				if err != nil {
					return mysqlJSONWireValue{}, err
				}
				items = append(items, item)
			}
			if err := readMySQLJSONWireClosingDelimiter(decoder, ']'); err != nil {
				return mysqlJSONWireValue{}, err
			}
			return mysqlJSONWireValue{kind: mysqlJSONWireArray, array: items}, nil
		default:
			return mysqlJSONWireValue{}, fmt.Errorf("unexpected JSON delimiter %q", typed)
		}
	default:
		return mysqlJSONWireValue{}, fmt.Errorf("unexpected JSON token type %T", token)
	}
}

func readMySQLJSONWireClosingDelimiter(decoder *json.Decoder, expected json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != expected {
		return fmt.Errorf("unexpected JSON closing token %v", token)
	}
	return nil
}

func validateMySQLPreviewWire(value mysqlJSONWireValue) error {
	fields, err := mysqlJSONExactObject(value, "preview", "version", "text", "fields", "truncated")
	if err != nil {
		return err
	}
	if fields["version"].kind != mysqlJSONWireNumber || fields["text"].kind != mysqlJSONWireString ||
		fields["truncated"].kind != mysqlJSONWireBool {
		return mysqlCodecInvalid("malformed MySQL preview envelope fields")
	}
	return validateMySQLTypedNodeWire(fields["fields"])
}

func validateMySQLTypedNodeWire(value mysqlJSONWireValue) error {
	fields, err := mysqlJSONUniqueObject(value, "typed preview node")
	if err != nil {
		return err
	}
	kindValue, kindPresent := fields["kind"]
	nilValue, nilPresent := fields["nil"]
	if !kindPresent || kindValue.kind != mysqlJSONWireString || !nilPresent || nilValue.kind != mysqlJSONWireBool {
		return mysqlCodecInvalid("typed preview node requires exact kind and nil members")
	}

	expected := []string{"kind", "nil"}
	if kindValue.stringValue == "nil" {
		if !nilValue.boolValue {
			return mysqlCodecInvalid("nil preview node requires nil true")
		}
		return mysqlJSONRequireExactFields(fields, "nil preview node", expected...)
	}

	if nilValue.boolValue {
		switch kindValue.stringValue {
		case "strings", "items", "map":
			return mysqlJSONRequireExactFields(fields, "nil preview container", expected...)
		default:
			return mysqlCodecInvalid("scalar or unknown preview node cannot be nil")
		}
	}

	var valueField string
	switch kindValue.stringValue {
	case "string":
		valueField = "string"
	case "bool":
		valueField = "bool"
	case "float64":
		valueField = "float_bits"
	case "int64":
		valueField = "int"
	case "strings":
		valueField = "strings"
	case "items":
		valueField = "items"
	case "map":
		valueField = "entries"
	default:
		return mysqlCodecInvalid("unknown MySQL preview value kind")
	}
	expected = append(expected, valueField)
	if err := mysqlJSONRequireExactFields(fields, "preview node", expected...); err != nil {
		return err
	}
	encodedValue := fields[valueField]
	switch kindValue.stringValue {
	case "string", "float64", "int64":
		if encodedValue.kind != mysqlJSONWireString {
			return mysqlCodecInvalid("preview scalar string member is missing or null")
		}
	case "bool":
		if encodedValue.kind != mysqlJSONWireBool {
			return mysqlCodecInvalid("preview bool member is missing or null")
		}
	case "strings":
		if encodedValue.kind != mysqlJSONWireArray {
			return mysqlCodecInvalid("preview strings member is missing or null")
		}
		for _, item := range encodedValue.array {
			if item.kind != mysqlJSONWireString {
				return mysqlCodecInvalid("preview strings element is null or not a string")
			}
		}
	case "items":
		if encodedValue.kind != mysqlJSONWireArray {
			return mysqlCodecInvalid("preview items member is missing or null")
		}
		for _, item := range encodedValue.array {
			if err := validateMySQLTypedNodeWire(item); err != nil {
				return err
			}
		}
	case "map":
		if encodedValue.kind != mysqlJSONWireArray {
			return mysqlCodecInvalid("preview entries member is missing or null")
		}
		for _, entry := range encodedValue.array {
			entryFields, err := mysqlJSONExactObject(entry, "preview map entry", "key", "value")
			if err != nil {
				return err
			}
			if entryFields["key"].kind != mysqlJSONWireString {
				return mysqlCodecInvalid("preview map key is null or not a string")
			}
			if err := validateMySQLTypedNodeWire(entryFields["value"]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMySQLLineageWire(value mysqlJSONWireValue) error {
	fields, err := mysqlJSONExactObject(value, "lineage", "version", "items")
	if err != nil {
		return err
	}
	if fields["version"].kind != mysqlJSONWireNumber || fields["items"].kind != mysqlJSONWireArray {
		return mysqlCodecInvalid("malformed MySQL lineage envelope fields")
	}
	for _, item := range fields["items"].array {
		itemFields, err := mysqlJSONExactObject(item, "lineage item", "artifact_ref", "relation")
		if err != nil {
			return err
		}
		if itemFields["artifact_ref"].kind != mysqlJSONWireString || itemFields["relation"].kind != mysqlJSONWireString {
			return mysqlCodecInvalid("lineage item contains a missing or null string field")
		}
	}
	return nil
}

func validateMySQLMetadataWire(value mysqlJSONWireValue) error {
	fields, err := mysqlJSONExactObject(value, "metadata", "version", "entries")
	if err != nil {
		return err
	}
	if fields["version"].kind != mysqlJSONWireNumber || fields["entries"].kind != mysqlJSONWireArray {
		return mysqlCodecInvalid("malformed MySQL metadata envelope fields")
	}
	for _, entry := range fields["entries"].array {
		entryFields, err := mysqlJSONExactObject(entry, "metadata entry", "key", "value")
		if err != nil {
			return err
		}
		if entryFields["key"].kind != mysqlJSONWireString || entryFields["value"].kind != mysqlJSONWireString {
			return mysqlCodecInvalid("metadata entry contains a missing or null string field")
		}
	}
	return nil
}

func mysqlJSONExactObject(value mysqlJSONWireValue, name string, expected ...string) (map[string]mysqlJSONWireValue, error) {
	fields, err := mysqlJSONUniqueObject(value, name)
	if err != nil {
		return nil, err
	}
	if err := mysqlJSONRequireExactFields(fields, name, expected...); err != nil {
		return nil, err
	}
	return fields, nil
}

func mysqlJSONUniqueObject(value mysqlJSONWireValue, name string) (map[string]mysqlJSONWireValue, error) {
	if value.kind != mysqlJSONWireObject {
		return nil, mysqlCodecInvalid(name + " must be a JSON object")
	}
	fields := make(map[string]mysqlJSONWireValue, len(value.object))
	for _, member := range value.object {
		if _, duplicate := fields[member.name]; duplicate {
			return nil, mysqlCodecInvalid("duplicate member in MySQL " + name)
		}
		fields[member.name] = member.value
	}
	return fields, nil
}

func mysqlJSONRequireExactFields(fields map[string]mysqlJSONWireValue, name string, expected ...string) error {
	allowed := make(map[string]struct{}, len(expected))
	for _, field := range expected {
		allowed[field] = struct{}{}
		if _, present := fields[field]; !present {
			return mysqlCodecInvalid("missing exact member " + field + " in MySQL " + name)
		}
	}
	if len(fields) != len(allowed) {
		return mysqlCodecInvalid("unexpected or case-variant member in MySQL " + name)
	}
	for field := range fields {
		if _, ok := allowed[field]; !ok {
			return mysqlCodecInvalid("unexpected or case-variant member " + field + " in MySQL " + name)
		}
	}
	return nil
}

func encodeMySQLString(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

func decodeMySQLString(value string) (string, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return "", mysqlCodecInvalidCause("invalid base64 in MySQL metadata payload", err)
	}
	if base64.StdEncoding.EncodeToString(decoded) != value {
		return "", mysqlCodecInvalidCause("non-canonical base64 in MySQL metadata payload", errMySQLNonCanonicalBase64)
	}
	return string(decoded), nil
}

func decodeMySQLJSONPayload(payload []byte, target any, name string) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return mysqlCodecInvalidCause("malformed MySQL "+name+" payload", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return mysqlCodecInvalid("trailing JSON in MySQL " + name + " payload")
		}
		return mysqlCodecInvalidCause("invalid trailing data in MySQL "+name+" payload", err)
	}
	return nil
}

func mysqlCodecInvalid(message string) error {
	return &artifact.Error{Code: artifact.ErrInvalidArgument, Message: message}
}

func mysqlCodecInvalidCause(message string, err error) error {
	return &artifact.Error{Code: artifact.ErrInvalidArgument, Message: message, Err: err}
}
