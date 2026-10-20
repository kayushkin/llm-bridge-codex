package main

// -generate-image mode: make images on the operator's Codex login (a ChatGPT
// subscription on this host), with no API key and no session.
//
// Contract, shared with llm-bridge-server's internal/server/image_generation.go:
// read a msg.ImageGenerationRequest JSON from stdin, write a
// msg.ImageGenerationResponse JSON to stdout, exit 0. On failure, write
// {"error":"..."} to stdout and exit 1.
//
// Measured against codex-cli 0.156.1 on 2026-10-13:
//
//   - Codex reaches its image tool through code mode. With code_mode_host
//     disabled, as -oneshot disables it, the model says the image tool is
//     unavailable and makes nothing. So this mode leaves code mode and
//     image_generation on and switches off everything else -oneshot does:
//     the shell, apps, browsers, plugins, web search. The sandbox is
//     read-only on top of that.
//   - A reference image goes in with -i <path>, and the model edits or
//     follows it ("the same crab, now wearing a beret" kept the crab).
//   - Codex saves each image as a PNG under
//     $CODEX_HOME/generated_images/<thread id>/, --ephemeral or not. The
//     exec --json stream carries no item for the image itself, so the files
//     are found by the thread id thread.started reports.
//   - A turn took 39 to 60 seconds and made one 1254x1254 PNG of about 2.5 MB.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// imageGenerationDisabledFeatures are -oneshot's disabled features less the
// two the image tool needs: image_generation itself and code_mode_host, the
// code mode it is reached through.
var imageGenerationDisabledFeatures = func() []string {
	needed := map[string]bool{"image_generation": true, "code_mode_host": true}
	var disabled []string
	for _, feature := range oneShotDisabledFeatures {
		if !needed[feature] {
			disabled = append(disabled, feature)
		}
	}
	return disabled
}()

// imageGenerationInstructions tell the model driving the image tool to make
// what was asked and nothing else.
const imageGenerationInstructions = "You make images. Use your image generation tool to make exactly what the user asks for, following any attached reference images. Do not ask questions and do not do anything else. When the images are made, reply with one short sentence saying what you made."

func runGenerateImage() int {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeOneShotError("read stdin: " + err.Error())
		return 1
	}
	var request msg.ImageGenerationRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		writeOneShotError("decode request: " + err.Error())
		return 1
	}
	if strings.TrimSpace(request.Prompt) == "" {
		writeOneShotError("prompt required")
		return 1
	}
	for _, path := range request.ReferenceImagePaths {
		if !filepath.IsAbs(path) {
			writeOneShotError("reference image path is not absolute: " + path)
			return 1
		}
		info, err := os.Stat(path)
		if err != nil {
			writeOneShotError("reference image: " + err.Error())
			return 1
		}
		if !info.Mode().IsRegular() {
			writeOneShotError("reference image is not a regular file: " + path)
			return 1
		}
	}
	if id := os.Getenv("LLMBRIDGE_CREDENTIAL_ID"); id != "" {
		writeOneShotError("instance has a bound credential (" + id + "); -generate-image runs on the Codex login and does not resolve auth-store credentials")
		return 1
	}
	if os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("CODEX_API_KEY") != "" {
		writeOneShotError("OPENAI_API_KEY or CODEX_API_KEY is set in the harness environment; -generate-image exists to use the Codex login and refuses to guess which credential wins — unset it")
		return 1
	}
	generatedImagesDirectory, err := codexGeneratedImagesDirectory()
	if err != nil {
		writeOneShotError(err.Error())
		return 1
	}
	cfg := loadConfig()
	bin, err := exec.LookPath(cfg.CodexPath)
	if err != nil {
		writeOneShotError(fmt.Sprintf("codex binary not found at %q: %v", cfg.CodexPath, err))
		return 1
	}
	workDir, err := oneShotWorkingDirectory()
	if err != nil {
		writeOneShotError("working directory: " + err.Error())
		return 1
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		writeOneShotError("create working directory: " + err.Error())
		return 1
	}

	instructions, _ := json.Marshal(imageGenerationInstructions)
	args := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
		"-s", "read-only", "-C", workDir, "-c", `web_search="disabled"`, "-c", "developer_instructions=" + string(instructions)}
	if cfg.CodexModel != "" {
		args = append(args, "-m", cfg.CodexModel)
	}
	for _, feature := range imageGenerationDisabledFeatures {
		args = append(args, "--disable", feature)
	}
	for _, path := range request.ReferenceImagePaths {
		args = append(args, "-i", path)
	}
	args = append(args, "-")

	ctx, cancel := context.WithTimeout(context.Background(), oneShotTimeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(request.Prompt)
	cmd.Dir = workDir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		writeOneShotError(fmt.Sprintf("codex exec exceeded %s", oneShotTimeout))
		return 1
	}
	result, err := readExecEvents(out)
	if err != nil {
		writeOneShotError(fmt.Sprintf("codex exec: %v (stderr: %s)", err, truncateForError(stderr.String())))
		return 1
	}
	if runErr != nil {
		writeOneShotError(fmt.Sprintf("codex exec: %v (stderr: %s)", runErr, truncateForError(stderr.String())))
		return 1
	}
	if result.threadID == "" {
		writeOneShotError("codex exec reported no thread id, so its images cannot be found")
		return 1
	}
	images, err := imagesCodexSaved(filepath.Join(generatedImagesDirectory, result.threadID))
	if err != nil {
		writeOneShotError(fmt.Sprintf("%v (the model said: %s)", err, truncateForError(result.lastMessage)))
		return 1
	}
	json.NewEncoder(os.Stdout).Encode(msg.ImageGenerationResponse{
		Images:     images,
		Text:       result.lastMessage,
		Usage:      result.usage,
		DurationMs: time.Since(start).Milliseconds(),
	})
	return 0
}

// codexGeneratedImagesDirectory is where codex saves generated images:
// $CODEX_HOME/generated_images, CODEX_HOME being ~/.codex when unset, as
// codex itself defaults it.
func codexGeneratedImagesDirectory() (string, error) {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the codex home: %w", err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	return filepath.Join(codexHome, "generated_images"), nil
}

// imagesCodexSaved lists the image files in one thread's directory, oldest
// first. A thread that saved none is an error: the model was asked for an
// image and did not make one.
func imagesCodexSaved(threadDirectory string) ([]msg.GeneratedImage, error) {
	entries, err := os.ReadDir(threadDirectory)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("the turn saved no image (%s does not exist)", threadDirectory)
	}
	if err != nil {
		return nil, err
	}
	type savedImage struct {
		image    msg.GeneratedImage
		modified time.Time
	}
	var saved []savedImage
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(threadDirectory, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		mediaType, err := mediaTypeOfFile(path)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(mediaType, "image/") {
			continue
		}
		saved = append(saved, savedImage{
			image:    msg.GeneratedImage{Path: path, MediaType: mediaType, SizeBytes: info.Size()},
			modified: info.ModTime(),
		})
	}
	if len(saved) == 0 {
		return nil, fmt.Errorf("the turn saved no image in %s", threadDirectory)
	}
	sort.SliceStable(saved, func(i, j int) bool { return saved[i].modified.Before(saved[j].modified) })
	images := make([]msg.GeneratedImage, len(saved))
	for index, image := range saved {
		images[index] = image.image
	}
	return images, nil
}

// mediaTypeOfFile reads a file's type from its first bytes.
func mediaTypeOfFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	head := make([]byte, 512)
	read, err := file.Read(head)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return http.DetectContentType(head[:read]), nil
}
