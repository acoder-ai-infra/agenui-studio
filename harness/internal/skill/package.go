package skill

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

const (
	// These transport limits follow the portable Agent Skills ZIP baseline used
	// by Azure OpenAI Skills: 50 MiB archives, 500 files and 25 MiB per file.
	DefaultMaxPackageBytes             int64 = 50 << 20
	DefaultMaxPackageFiles                   = 500
	DefaultMaxPackageFileBytes         int64 = 25 << 20
	DefaultMaxPackageUncompressedBytes int64 = 100 << 20
	DefaultMaxPackageObjectBytes       int64 = DefaultMaxPackageBytes
)

var ErrInvalidPackage = errors.New("invalid skill package")

type PackageInspection struct {
	Definition       Definition       `json:"definition"`
	SkillPath        string           `json:"skill_path"`
	ArchiveSize      int64            `json:"archive_size"`
	FileCount        int              `json:"file_count"`
	UncompressedSize int64            `json:"uncompressed_size"`
	Files            []FileRecord     `json:"files,omitempty"`
	Warnings         []PackageWarning `json:"warnings,omitempty"`
	Content          []byte           `json:"-"`
}

type PackageWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type packageFrontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	Metadata      map[string]any    `yaml:"metadata,omitempty"`
	AllowedTools  allowedToolsValue `yaml:"allowed-tools,omitempty"`
}

type allowedToolsValue []string

func (v *allowedToolsValue) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case 0:
		return nil
	case yaml.ScalarNode:
		*v = append((*v)[:0], strings.Fields(node.Value)...)
		return nil
	case yaml.SequenceNode:
		var values []string
		if err := node.Decode(&values); err != nil {
			return err
		}
		for index, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				return fmt.Errorf("allowed-tools contains an empty value")
			}
			values[index] = value
		}
		*v = values
		return nil
	default:
		return fmt.Errorf("allowed-tools must be a space-delimited string or a string array")
	}
}

func InspectPackage(archive []byte) (PackageInspection, error) {
	if len(archive) == 0 || int64(len(archive)) > DefaultMaxPackageBytes {
		return PackageInspection{}, fmt.Errorf("%w: ZIP size must be between 1 byte and %d bytes", ErrInvalidPackage, DefaultMaxPackageBytes)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return PackageInspection{}, fmt.Errorf("%w: unreadable ZIP: %v", ErrInvalidPackage, err)
	}

	inspection := PackageInspection{ArchiveSize: int64(len(archive))}
	root := ""
	seenPaths := make(map[string]struct{}, len(reader.File))
	var skillFile *zip.File
	for _, file := range reader.File {
		name, ignored, err := validatePackagePath(file)
		if err != nil {
			return PackageInspection{}, err
		}
		if ignored || file.FileInfo().IsDir() {
			continue
		}
		if _, exists := seenPaths[name]; exists {
			return PackageInspection{}, fmt.Errorf("%w: duplicate package path %s", ErrInvalidPackage, name)
		}
		seenPaths[name] = struct{}{}
		parts := strings.Split(name, "/")
		if len(parts) < 2 || parts[0] == "" {
			return PackageInspection{}, fmt.Errorf("%w: every file must be inside one top-level skill folder", ErrInvalidPackage)
		}
		if root == "" {
			root = parts[0]
		} else if root != parts[0] {
			return PackageInspection{}, fmt.Errorf("%w: ZIP must contain one top-level skill folder", ErrInvalidPackage)
		}
		inspection.FileCount++
		if inspection.FileCount > DefaultMaxPackageFiles {
			return PackageInspection{}, fmt.Errorf("%w: package contains more than %d files", ErrInvalidPackage, DefaultMaxPackageFiles)
		}
		if file.UncompressedSize64 > uint64(DefaultMaxPackageFileBytes) {
			return PackageInspection{}, fmt.Errorf("%w: %s exceeds the %d byte per-file limit", ErrInvalidPackage, name, DefaultMaxPackageFileBytes)
		}
		if file.UncompressedSize64 > uint64(DefaultMaxPackageUncompressedBytes-inspection.UncompressedSize) {
			return PackageInspection{}, fmt.Errorf("%w: package exceeds the %d byte uncompressed limit", ErrInvalidPackage, DefaultMaxPackageUncompressedBytes)
		}
		inspection.UncompressedSize += int64(file.UncompressedSize64)
		if strings.EqualFold(path.Base(name), "SKILL.md") {
			if len(parts) != 2 {
				return PackageInspection{}, fmt.Errorf("%w: SKILL.md must be directly inside the top-level skill folder", ErrInvalidPackage)
			}
			if skillFile != nil {
				return PackageInspection{}, fmt.Errorf("%w: package must contain exactly one SKILL.md", ErrInvalidPackage)
			}
			skillFile = file
			inspection.SkillPath = name
		}
	}
	if skillFile == nil {
		return PackageInspection{}, fmt.Errorf("%w: package must contain exactly one SKILL.md", ErrInvalidPackage)
	}
	if err := validatePackageContents(reader.File); err != nil {
		return PackageInspection{}, err
	}
	content, err := readPackageFile(skillFile, DefaultMaxContentBytes)
	if err != nil {
		return PackageInspection{}, err
	}
	files, err := readPackageFiles(reader.File, root)
	if err != nil {
		return PackageInspection{}, err
	}
	definition, warnings, err := parsePackageDefinition(content, root)
	if err != nil {
		return PackageInspection{}, err
	}
	inspection.Definition = definition
	inspection.Warnings = warnings
	inspection.Content = content
	inspection.Files = files
	return inspection, nil
}

func validatePackagePath(file *zip.File) (string, bool, error) {
	name := strings.TrimSpace(file.Name)
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", false, fmt.Errorf("%w: unsafe package path %q", ErrInvalidPackage, file.Name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || (!file.FileInfo().IsDir() && clean != name) {
		return "", false, fmt.Errorf("%w: unsafe package path %q", ErrInvalidPackage, file.Name)
	}
	if file.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("%w: symbolic links are not allowed: %s", ErrInvalidPackage, clean)
	}
	ignored := strings.HasPrefix(clean, "__MACOSX/") || path.Base(clean) == ".DS_Store"
	return clean, ignored, nil
}

func validatePackageContents(files []*zip.File) error {
	for _, file := range files {
		_, ignored, err := validatePackagePath(file)
		if err != nil {
			return err
		}
		if ignored || file.FileInfo().IsDir() {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return fmt.Errorf("%w: open %s: %v", ErrInvalidPackage, file.Name, err)
		}
		read, copyErr := io.Copy(io.Discard, io.LimitReader(reader, DefaultMaxPackageFileBytes+1))
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil || read != int64(file.UncompressedSize64) {
			return fmt.Errorf("%w: corrupt or oversized entry %s", ErrInvalidPackage, file.Name)
		}
	}
	return nil
}

func readPackageFile(file *zip.File, limit int64) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %v", ErrInvalidPackage, file.Name, err)
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrInvalidPackage, file.Name, err)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%w: SKILL.md exceeds the %d byte instruction limit", ErrInvalidPackage, limit)
	}
	if !utf8.Valid(content) {
		return nil, fmt.Errorf("%w: SKILL.md must be valid UTF-8", ErrInvalidPackage)
	}
	return content, nil
}

func readPackageFiles(files []*zip.File, root string) ([]FileRecord, error) {
	result := make([]FileRecord, 0)
	for _, file := range files {
		name, ignored, err := validatePackagePath(file)
		if err != nil {
			return nil, err
		}
		if ignored || file.FileInfo().IsDir() {
			continue
		}
		content, err := readPackageFileBytes(file, DefaultMaxPackageFileBytes)
		if err != nil {
			return nil, err
		}
		filePath := strings.TrimPrefix(name, root+"/")
		result = append(result, FileRecord{
			Path:        filePath,
			MimeType:    packageFileMimeType(filePath),
			SizeBytes:   int64(len(content)),
			ContentHash: contentHash(content),
			Previewable: utf8.Valid(content),
			Content:     content,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path == "SKILL.md" {
			return true
		}
		if result[j].Path == "SKILL.md" {
			return false
		}
		return result[i].Path < result[j].Path
	})
	return result, nil
}

func readPackageFileBytes(file *zip.File, limit int64) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %v", ErrInvalidPackage, file.Name, err)
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrInvalidPackage, file.Name, err)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%w: %s exceeds the %d byte per-file limit", ErrInvalidPackage, file.Name, limit)
	}
	return content, nil
}

func packageFileMimeType(filePath string) string {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".yaml", ".yml":
		return "application/yaml; charset=utf-8"
	case ".txt", ".text", ".csv", ".log":
		return "text/plain; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func parsePackageDefinition(content []byte, parent string) (Definition, []PackageWarning, error) {
	frontmatterBytes, body, err := splitPackageFrontmatter(content)
	if err != nil {
		return Definition{}, nil, err
	}
	frontmatter, extensionFields, err := decodePackageFrontmatter(frontmatterBytes)
	if err != nil {
		return Definition{}, nil, err
	}
	if err := validatePackageFrontmatter(frontmatter); err != nil {
		return Definition{}, nil, err
	}
	if strings.TrimSpace(string(body)) == "" {
		return Definition{}, nil, fmt.Errorf("%w: SKILL.md instructions are empty", ErrInvalidPackage)
	}
	version, err := normalizePackageVersion(metadataString(frontmatter.Metadata, "version"))
	if err != nil {
		return Definition{}, nil, err
	}
	strategy := InjectSystem
	if value := metadataString(frontmatter.Metadata, "injection_strategy"); value != "" {
		strategy = InjectionStrategy(value)
		if strategy != InjectSystem && strategy != InjectOnDemand && strategy != InjectToolOnly {
			return Definition{}, nil, fmt.Errorf("%w: unsupported injection_strategy %q", ErrInvalidPackage, value)
		}
	}
	warnings := packageFrontmatterWarnings(frontmatter.Name, parent, extensionFields)
	return Definition{
		ID: frontmatter.Name, Version: version, Description: frontmatter.Description,
		InjectionStrategy: strategy,
		Dependencies:      Dependencies{Tools: append([]string(nil), frontmatter.AllowedTools...)},
		Policy:            Policy{Scope: ScopeTenant},
	}, warnings, nil
}

func decodePackageFrontmatter(content []byte) (packageFrontmatter, []string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return packageFrontmatter{}, nil, fmt.Errorf("%w: parse SKILL.md frontmatter: %v", ErrInvalidPackage, err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return packageFrontmatter{}, nil, fmt.Errorf("%w: SKILL.md frontmatter must be a YAML mapping", ErrInvalidPackage)
	}
	mapping := document.Content[0]
	var frontmatter packageFrontmatter
	if err := mapping.Decode(&frontmatter); err != nil {
		return packageFrontmatter{}, nil, fmt.Errorf("%w: parse SKILL.md frontmatter: %v", ErrInvalidPackage, err)
	}
	known := map[string]struct{}{
		"name": {}, "description": {}, "license": {}, "compatibility": {}, "metadata": {}, "allowed-tools": {},
	}
	seenExtensions := make(map[string]struct{})
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		key := strings.TrimSpace(mapping.Content[index].Value)
		if _, ok := known[key]; ok || key == "" {
			continue
		}
		seenExtensions[key] = struct{}{}
	}
	extensions := make([]string, 0, len(seenExtensions))
	for key := range seenExtensions {
		extensions = append(extensions, key)
	}
	sort.Strings(extensions)
	return frontmatter, extensions, nil
}

func packageFrontmatterWarnings(name, parent string, extensionFields []string) []PackageWarning {
	warnings := make([]PackageWarning, 0, 2)
	if len(extensionFields) > 0 {
		warnings = append(warnings, PackageWarning{
			Code:    "non_standard_frontmatter_fields",
			Message: "SKILL.md 包含非标准顶层字段，已保留但不参与平台配置：" + strings.Join(extensionFields, "、"),
		})
	}
	if name != parent {
		warnings = append(warnings, PackageWarning{
			Code:    "skill_name_directory_mismatch",
			Message: fmt.Sprintf("Skill name %q 与目录 %q 不一致；发布时使用 name 作为 Skill ID", name, parent),
		})
	}
	return warnings
}

func splitPackageFrontmatter(content []byte) ([]byte, []byte, error) {
	normalized := bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(normalized, []byte("---\n")) {
		return nil, nil, fmt.Errorf("%w: SKILL.md must start with YAML frontmatter", ErrInvalidPackage)
	}
	end := bytes.Index(normalized[4:], []byte("\n---\n"))
	if end < 0 {
		return nil, nil, fmt.Errorf("%w: SKILL.md frontmatter is not closed", ErrInvalidPackage)
	}
	end += 4
	return normalized[4:end], normalized[end+5:], nil
}

func validatePackageFrontmatter(frontmatter packageFrontmatter) error {
	name := frontmatter.Name
	if len(name) == 0 || len(name) > 64 || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return fmt.Errorf("%w: name must use 1-64 lowercase letters, numbers or single hyphens", ErrInvalidPackage)
	}
	for _, character := range name {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return fmt.Errorf("%w: name must use lowercase letters, numbers and hyphens", ErrInvalidPackage)
		}
	}
	descriptionLength := len([]rune(frontmatter.Description))
	if descriptionLength == 0 || descriptionLength > 1024 {
		return fmt.Errorf("%w: description must contain 1-1024 characters", ErrInvalidPackage)
	}
	if len([]rune(frontmatter.Compatibility)) > 500 {
		return fmt.Errorf("%w: compatibility must not exceed 500 characters", ErrInvalidPackage)
	}
	return nil
}

func metadataString(metadata map[string]any, key string) string {
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func normalizePackageVersion(value string) (string, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if value == "" {
		value = "1.0.0"
	}
	switch strings.Count(value, ".") {
	case 0:
		value += ".0.0"
	case 1:
		value += ".0"
	}
	if !semver.IsValid("v" + value) {
		return "", fmt.Errorf("%w: metadata.version must be a semantic version", ErrInvalidPackage)
	}
	return value, nil
}
