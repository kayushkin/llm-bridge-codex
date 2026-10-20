package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A one-pixel PNG, so http.DetectContentType reads image/png.
var onePixelPNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89}

func TestImageGenerationLeavesOnOnlyTheImageToolAndCodeMode(t *testing.T) {
	for _, needed := range []string{"image_generation", "code_mode_host"} {
		if slices.Contains(imageGenerationDisabledFeatures, needed) {
			t.Errorf("%s is disabled, but the image tool needs it", needed)
		}
	}
	for _, feature := range oneShotDisabledFeatures {
		if feature == "image_generation" || feature == "code_mode_host" {
			continue
		}
		if !slices.Contains(imageGenerationDisabledFeatures, feature) {
			t.Errorf("%s is disabled for -oneshot but left on for -generate-image", feature)
		}
	}
}

func TestImagesCodexSavedListsImagesOldestFirstAndSkipsOtherFiles(t *testing.T) {
	directory := t.TempDir()
	write := func(name string, content []byte, modified time.Time) {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write("second.png", onePixelPNG, now)
	write("first.png", onePixelPNG, now.Add(-time.Minute))
	write("notes.txt", []byte("not an image"), now)

	images, err := imagesCodexSaved(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 {
		t.Fatalf("got %d images, want 2: %+v", len(images), images)
	}
	if filepath.Base(images[0].Path) != "first.png" || filepath.Base(images[1].Path) != "second.png" {
		t.Errorf("order = %s, %s; want first.png, second.png", images[0].Path, images[1].Path)
	}
	if images[0].MediaType != "image/png" || images[0].SizeBytes != int64(len(onePixelPNG)) {
		t.Errorf("first image = %+v", images[0])
	}
}

func TestImagesCodexSavedRefusesAThreadThatSavedNothing(t *testing.T) {
	if _, err := imagesCodexSaved(filepath.Join(t.TempDir(), "no-such-thread")); err == nil {
		t.Error("a missing thread directory gave no error")
	}
	if _, err := imagesCodexSaved(t.TempDir()); err == nil {
		t.Error("an empty thread directory gave no error")
	}
}
