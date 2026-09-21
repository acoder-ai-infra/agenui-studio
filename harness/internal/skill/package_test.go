package skill

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestInspectPackageReadsStandardSkillArchive(t *testing.T) {
	archive := testSkillArchive(t, map[string]string{
		"expense-report/SKILL.md": `---
name: expense-report
description: Review and submit employee expense reports.
metadata:
  version: "2.1"
  injection_strategy: system_inject
allowed-tools:
  - finance.read@1.0.0
---
# Expense reports

Follow the company expense policy.
`,
		"expense-report/references/policy.md": "# Policy",
	})

	inspection, err := InspectPackage(archive)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Definition.ID != "expense-report" || inspection.Definition.Version != "2.1.0" {
		t.Fatalf("definition identity = %#v", inspection.Definition)
	}
	if inspection.Definition.Description != "Review and submit employee expense reports." || inspection.Definition.InjectionStrategy != InjectSystem {
		t.Fatalf("definition metadata = %#v", inspection.Definition)
	}
	if got := inspection.Definition.Dependencies.Tools; len(got) != 1 || got[0] != "finance.read@1.0.0" {
		t.Fatalf("allowed-tools = %#v", got)
	}
	if inspection.SkillPath != "expense-report/SKILL.md" || inspection.FileCount != 2 || inspection.ArchiveSize != int64(len(archive)) {
		t.Fatalf("inspection summary = %#v", inspection)
	}
	if len(inspection.Files) != 2 || inspection.Files[0].Path != "SKILL.md" || inspection.Files[1].Path != "references/policy.md" {
		t.Fatalf("inspection files = %#v", inspection.Files)
	}
	if inspection.Files[1].MimeType != "text/markdown; charset=utf-8" || !inspection.Files[1].Previewable || !bytes.Equal(inspection.Files[1].Content, []byte("# Policy")) {
		t.Fatalf("reference file = %#v content=%q", inspection.Files[1], inspection.Files[1].Content)
	}
	if !bytes.Contains(inspection.Content, []byte("Follow the company expense policy.")) {
		t.Fatalf("SKILL.md content was not preserved: %q", inspection.Content)
	}
	if len(inspection.Warnings) != 0 {
		t.Fatalf("standard package warnings = %#v", inspection.Warnings)
	}
}

func TestInspectPackageUsesPortableDefaults(t *testing.T) {
	archive := testSkillArchive(t, map[string]string{
		"simple-skill/SKILL.md": `---
name: simple-skill
description: A small portable skill.
allowed-tools: read.file@1.0.0 search.web@1.0.0
---
Use the available tools when needed.
`,
	})

	inspection, err := InspectPackage(archive)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Definition.Version != "1.0.0" || inspection.Definition.InjectionStrategy != InjectSystem {
		t.Fatalf("portable defaults = %#v", inspection.Definition)
	}
	wantTools := []string{"read.file@1.0.0", "search.web@1.0.0"}
	if strings.Join(inspection.Definition.Dependencies.Tools, ",") != strings.Join(wantTools, ",") {
		t.Fatalf("allowed-tools = %#v, want %#v", inspection.Definition.Dependencies.Tools, wantTools)
	}
	if DefaultMaxPackageBytes != 50<<20 || DefaultMaxPackageFiles != 500 || DefaultMaxPackageFileBytes != 25<<20 || DefaultMaxPackageObjectBytes != 50<<20 {
		t.Fatalf("package limits drifted: bytes=%d files=%d file_bytes=%d object_bytes=%d", DefaultMaxPackageBytes, DefaultMaxPackageFiles, DefaultMaxPackageFileBytes, DefaultMaxPackageObjectBytes)
	}
}

func TestBuildPackageArchiveIsDeterministicAcrossFileOrder(t *testing.T) {
	content := []byte("instructions")
	files := []FileRecord{
		defaultFileRecord("SKILL.md", content),
		defaultFileRecord("references/policy.md", []byte("policy")),
	}
	first, err := buildPackageArchive("planner", files)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildPackageArchive("planner", []FileRecord{files[1], files[0]})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("canonical Skill ZIP changed when only input file order changed")
	}
	got, err := readPackageArchiveFile(first, "references/policy.md", files[1].ContentHash)
	if err != nil || string(got) != "policy" {
		t.Fatalf("read canonical package file=%q err=%v", got, err)
	}
}

func TestInspectPackagePreservesExplicitOnDemandInjection(t *testing.T) {
	archive := testSkillArchive(t, map[string]string{
		"callable-skill/SKILL.md": `---
name: callable-skill
description: A skill that declares a future execution binding.
metadata:
  injection_strategy: on_demand
---
Use this skill only when selected.
`,
	})

	inspection, err := InspectPackage(archive)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Definition.InjectionStrategy != InjectOnDemand {
		t.Fatalf("explicit injection strategy = %q, want %q", inspection.Definition.InjectionStrategy, InjectOnDemand)
	}
}

func TestInspectPackageRejectsUnsafeOrAmbiguousArchives(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		message string
	}{
		{
			name: "path traversal",
			files: map[string]string{
				"../SKILL.md": "---\nname: unsafe\ndescription: unsafe\n---\nbody",
			},
			message: "unsafe package path",
		},
		{
			name: "multiple skill files",
			files: map[string]string{
				"first/SKILL.md": "---\nname: first\ndescription: first\n---\nbody",
				"first/skill.MD": "---\nname: first\ndescription: duplicate\n---\nbody",
			},
			message: "exactly one SKILL.md",
		},
		{
			name: "invalid core name",
			files: map[string]string{
				"AccountBase/SKILL.md": "---\nname: AccountBase\ndescription: invalid name\n---\nbody",
			},
			message: "name must use lowercase letters",
		},
		{
			name: "missing description",
			files: map[string]string{
				"missing-description/SKILL.md": "---\nname: missing-description\n---\nbody",
			},
			message: "description must contain 1-1024 characters",
		},
		{
			name: "malformed frontmatter",
			files: map[string]string{
				"malformed/SKILL.md": "---\nname: malformed\ndescription: [not closed\n---\nbody",
			},
			message: "parse SKILL.md frontmatter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := InspectPackage(testSkillArchive(t, tt.files))
			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("error = %v, want message containing %q", err, tt.message)
			}
		})
	}

	t.Run("duplicate package path", func(t *testing.T) {
		archive := testSkillArchiveEntries(t, []testSkillArchiveEntry{
			{name: "duplicate/SKILL.md", content: "---\nname: duplicate\ndescription: duplicate\n---\nbody"},
			{name: "duplicate/reference.md", content: "first"},
			{name: "duplicate/reference.md", content: "second"},
		})
		_, err := InspectPackage(archive)
		if err == nil || !strings.Contains(err.Error(), "duplicate package path") {
			t.Fatalf("error = %v, want duplicate package path", err)
		}
	})

	t.Run("symbolic link", func(t *testing.T) {
		archive := testSkillArchiveEntries(t, []testSkillArchiveEntry{
			{name: "linked/SKILL.md", content: "---\nname: linked\ndescription: linked\n---\nbody"},
			{name: "linked/reference.md", content: "target", mode: os.ModeSymlink | 0o777},
		})
		_, err := InspectPackage(archive)
		if err == nil || !strings.Contains(err.Error(), "symbolic links are not allowed") {
			t.Fatalf("error = %v, want symbolic link rejection", err)
		}
	})
}

func TestInspectPackageAcceptsBusinessExtensionsWithWarnings(t *testing.T) {
	archive := testSkillArchive(t, map[string]string{
		"account-base-skill-v2/SKILL.md": `---
name: account-base
nameCh: 账号基础组件
description: Account foundation integration guidance.
descriptionCh: 账号基础组件接入指引
bizDomain: 用户中心
flowChart: |-
  graph LR
    login --> profile
capabilities:
  knowledge_qa: true
---
# 账号基础组件

根据业务场景选择账号接口。
`,
	})

	inspection, err := InspectPackage(archive)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Definition.ID != "account-base" {
		t.Fatalf("definition = %#v", inspection.Definition)
	}
	warnings := make(map[string]PackageWarning, len(inspection.Warnings))
	for _, warning := range inspection.Warnings {
		warnings[warning.Code] = warning
	}
	if warning, ok := warnings["non_standard_frontmatter_fields"]; !ok || !strings.Contains(warning.Message, "nameCh") || !strings.Contains(warning.Message, "capabilities") {
		t.Fatalf("frontmatter warning = %#v", warning)
	}
	if warning, ok := warnings["skill_name_directory_mismatch"]; !ok || !strings.Contains(warning.Message, "account-base-skill-v2") {
		t.Fatalf("directory warning = %#v", warning)
	}
	if !bytes.Contains(inspection.Content, []byte("bizDomain: 用户中心")) {
		t.Fatalf("business metadata was not preserved: %q", inspection.Content)
	}
}

func TestInspectPackageEnforcesEntryLimits(t *testing.T) {
	t.Run("file count", func(t *testing.T) {
		files := map[string]string{
			"crowded/SKILL.md": "---\nname: crowded\ndescription: crowded\n---\nbody",
		}
		for index := 0; index < DefaultMaxPackageFiles; index++ {
			files[fmt.Sprintf("crowded/references/%03d.md", index)] = "x"
		}
		_, err := InspectPackage(testSkillArchive(t, files))
		if err == nil || !strings.Contains(err.Error(), "more than 500 files") {
			t.Fatalf("error = %v, want file count rejection", err)
		}
	})

	t.Run("uncompressed file size", func(t *testing.T) {
		archive := testSkillArchive(t, map[string]string{
			"large/SKILL.md":             "---\nname: large\ndescription: large\n---\nbody",
			"large/references/large.txt": strings.Repeat("x", int(DefaultMaxPackageFileBytes)+1),
		})
		_, err := InspectPackage(archive)
		if err == nil || !strings.Contains(err.Error(), "per-file limit") {
			t.Fatalf("error = %v, want per-file size rejection", err)
		}
	})
}

func testSkillArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	entries := make([]testSkillArchiveEntry, 0, len(files))
	for name, content := range files {
		entries = append(entries, testSkillArchiveEntry{name: name, content: content})
	}
	return testSkillArchiveEntries(t, entries)
}

type testSkillArchiveEntry struct {
	name    string
	content string
	mode    os.FileMode
}

func testSkillArchiveEntries(t *testing.T, entries []testSkillArchiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, item := range entries {
		header := &zip.FileHeader{Name: item.name, Method: zip.Deflate}
		if item.mode != 0 {
			header.SetMode(item.mode)
		}
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(item.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
