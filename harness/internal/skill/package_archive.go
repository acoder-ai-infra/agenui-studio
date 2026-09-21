package skill

import (
	"archive/zip"
	"bytes"
	"fmt"
	"path"
	"sort"
	"time"
)

var deterministicZipTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

func buildPackageArchive(skillID string, files []FileRecord) ([]byte, error) {
	files = append([]FileRecord(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, file := range files {
		filePath, err := normalizePreviewPath(file.Path)
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		header := &zip.FileHeader{Name: path.Join(skillID, filePath), Method: zip.Deflate}
		header.SetModTime(deterministicZipTime)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("build skill package: %w", err)
		}
		if _, err := entry.Write(file.Content); err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("build skill package file %s: %w", file.Path, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("build skill package: %w", err)
	}
	if int64(buffer.Len()) > DefaultMaxPackageObjectBytes {
		return nil, fmt.Errorf("%w: normalized ZIP exceeds the %d byte object limit", ErrInvalidPackage, DefaultMaxPackageObjectBytes)
	}
	return buffer.Bytes(), nil
}

func readPackageArchiveFile(archive []byte, requestedPath, expectedHash string) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable persisted ZIP: %v", ErrContentIntegrity, err)
	}
	for _, file := range reader.File {
		name, ignored, err := validatePackagePath(file)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrContentIntegrity, err)
		}
		if ignored || file.FileInfo().IsDir() {
			continue
		}
		parts := bytes.SplitN([]byte(name), []byte{'/'}, 2)
		if len(parts) != 2 || string(parts[1]) != requestedPath {
			continue
		}
		content, err := readPackageFileBytes(file, DefaultMaxPackageFileBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrContentIntegrity, err)
		}
		if contentHash(content) != expectedHash {
			return nil, fmt.Errorf("%w: persisted package file hash mismatch: %s", ErrContentIntegrity, requestedPath)
		}
		return content, nil
	}
	return nil, ErrNotFound
}
