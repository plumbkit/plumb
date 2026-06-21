package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteFile_RefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"file_path": sub})
	_, err := NewDeleteFile(WriteDeps{}).Execute(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("expected directory error, got: %v", err)
	}
}

func TestDeleteFile_AllowDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"file_path": sub, "allow_dir": true})
	out, err := NewDeleteFile(WriteDeps{}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "deleted") {
		t.Errorf("unexpected output: %q", out)
	}
	if _, statErr := os.Stat(sub); !os.IsNotExist(statErr) {
		t.Error("directory still exists after deletion")
	}
}

func TestDeleteFile_AllowDir_NonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nonempty")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"file_path": sub, "allow_dir": true})
	_, err := NewDeleteFile(WriteDeps{}).Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for non-empty directory, got nil")
	}
}

func TestDeleteFile_AllowDir_WithoutFlag(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// allow_dir not set — should still refuse even if empty
	raw, _ := json.Marshal(map[string]any{"file_path": sub})
	_, err := NewDeleteFile(WriteDeps{}).Execute(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("expected directory error without allow_dir, got: %v", err)
	}
}

func TestDeleteFile_SummaryReportsLinesAndBytes(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "code.txt")
	content := "one\ntwo\nthree\n" // 3 lines, 14 bytes
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"file_path": f})
	out, err := NewDeleteFile(WriteDeps{}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "3 lines") || !strings.Contains(out, "14 bytes removed") {
		t.Errorf("expected line+byte summary, got: %q", out)
	}
}

func TestDeleteFile_SummaryBinaryFallsBackToBytes(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(f, []byte{0x00, 0x01, 0x02, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"file_path": f})
	out, err := NewDeleteFile(WriteDeps{}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "binary") || strings.Contains(out, "lines") {
		t.Errorf("expected binary byte-only summary, got: %q", out)
	}
}
