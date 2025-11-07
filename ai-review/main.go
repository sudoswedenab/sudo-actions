package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
)

const (
	defaultModel         = "gpt-4.1-mini"
	defaultMaxFiles      = 80
	fileSizeLimitBytes   = 200_000
	requestTimeout       = 120 * time.Second
	reviewCommentTimeout = 60 * time.Second
)

var binaryExtensions = []string{".png", ".jpg", ".jpeg", ".pdf", ".zip", ".exe", ".bin", ".mp4"}

var repoRootDir string

type config struct {
	Model               string `yaml:"model"`
	MaxFiles            int    `yaml:"max_files"`
	RejectLargeBinaries *bool  `yaml:"reject_large_binaries"`
	InlineComments      *bool  `yaml:"inline_comments"`
}

type filePayload struct {
	Path     string `json:"path"`
	Language string `json:"language"`
	Content  string `json:"content"`
}

type prMeta struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Number  int    `json:"number"`
	BaseRef string `json:"base_ref"`
	HeadRef string `json:"head_ref"`
	HTMLURL string `json:"html_url"`
	User    string `json:"user"`
}

type reviewResult struct {
	Summary         string    `json:"summary"`
	RepoSuggestions []string  `json:"repo_suggestions"`
	Findings        []finding `json:"findings"`
}

type finding struct {
	Title          string `json:"title"`
	Priority       string `json:"priority"`
	File           string `json:"file"`
	Details        string `json:"details"`
	StartLine      int    `json:"start_line"`
	EndLine        int    `json:"end_line"`
	SuggestedPatch string `json:"suggested_patch"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ai-review: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	actionRoot := resolveActionRoot()
	repoRoot := resolveRepoRoot()
	repoRootDir = repoRoot

	cfg := loadConfig(filepath.Join(actionRoot, "ai-review.yml"))
	systemPrompt, err := loadSystemPrompt(filepath.Join(actionRoot, "prompts", "AGENT_INSTRUCTION.md"))
	if err != nil {
		return err
	}

	files, err := collectFiles(repoRoot, cfg)
	if err != nil {
		return err
	}

	pr := readPRMeta()

	userPrompt, err := buildUserPrompt(files, pr)
	if err != nil {
		return err
	}

	raw, err := openAIRequest(systemPrompt, userPrompt, cfg.Model)
	if err != nil {
		return err
	}

	result, err := parseReviewResult(raw)
	if err != nil {
		result = reviewResult{
			Summary: "Could not parse AI response as JSON.",
		}
	}

	if err := postReviewComment(result); err != nil {
		return err
	}

	if cfg.inlineCommentsEnabled() {
		if err := postInlineComments(result.Findings); err != nil {
			return err
		}
	}

	return nil
}

func resolveActionRoot() string {
	if p := os.Getenv("GITHUB_ACTION_PATH"); p != "" {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func resolveRepoRoot() string {
	if p := os.Getenv("GITHUB_WORKSPACE"); p != "" {
		return p
	}
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

func loadConfig(path string) config {
	cfg := config{
		Model:    defaultModel,
		MaxFiles: defaultMaxFiles,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ai-review: failed to parse config: %v\n", err)
		return cfg
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = defaultMaxFiles
	}
	return cfg
}

func (c config) rejectBinariesEnabled() bool {
	if c.RejectLargeBinaries == nil {
		return true
	}
	return *c.RejectLargeBinaries
}

func (c config) inlineCommentsEnabled() bool {
	if c.InlineComments == nil {
		return true
	}
	return *c.InlineComments
}

func loadSystemPrompt(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return string(data), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return "You are a rigorous code reviewer.", nil
}

func collectFiles(repoRoot string, cfg config) ([]filePayload, error) {
	paths, err := getChangedFiles()
	if err != nil {
		return nil, err
	}
	if len(paths) > cfg.MaxFiles {
		paths = paths[:cfg.MaxFiles]
	}

	var collected []filePayload
	for _, p := range paths {
		if cfg.rejectBinariesEnabled() && hasBinaryExtension(p) {
			continue
		}
		content, err := loadText(repoRoot, p)
		if err != nil {
			continue
		}
		collected = append(collected, filePayload{
			Path:     p,
			Language: detectLang(p),
			Content:  content,
		})
	}
	return collected, nil
}

func getChangedFiles() ([]string, error) {
	baseRef := os.Getenv("GITHUB_BASE_REF")
	diffRange := "HEAD~1...HEAD"

	if baseRef != "" {
		remoteRef := "origin/" + baseRef
		if _, err := runCommand("git", "fetch", "origin", baseRef, "--depth=1"); err != nil {
			fmt.Fprintf(os.Stderr, "ai-review: warning: git fetch failed: %v\n", err)
		} else if base, err := runCommand("git", "merge-base", remoteRef, "HEAD"); err == nil {
			diffRange = strings.TrimSpace(base) + "...HEAD"
		}
	}

	out, err := runCommand("git", "diff", "--name-only", diffRange)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	var files []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = os.Environ()
	if repoRootDir != "" {
		cmd.Dir = repoRootDir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, string(output))
	}
	return string(output), nil
}

func hasBinaryExtension(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range binaryExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func loadText(repoRoot, relPath string) (string, error) {
	full := filepath.Join(repoRoot, relPath)
	info, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("is directory")
	}
	if info.Size() > fileSizeLimitBytes {
		return "", fmt.Errorf("file too large")
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func detectLang(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		return "python"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".js":
		return "javascript"
	case ".jsx":
		return "jsx"
	case ".go":
		return "go"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".rs":
		return "rust"
	case ".cpp":
		return "cpp"
	case ".c", ".h":
		return "c"
	case ".yml", ".yaml":
		return "yaml"
	case ".json":
		return "json"
	case ".md":
		return "markdown"
	case ".cue":
		return "cue"
	default:
		return "text"
	}
}

func readPRMeta() prMeta {
	eventPath := os.Getenv("GITHUB_EVENT_PATH")
	if eventPath == "" {
		return prMeta{}
	}
	data, err := os.ReadFile(eventPath)
	if err != nil {
		return prMeta{}
	}
	var evt map[string]any
	if err := json.Unmarshal(data, &evt); err != nil {
		return prMeta{}
	}
	pr, _ := evt["pull_request"].(map[string]any)
	if pr == nil {
		return prMeta{}
	}
	return prMeta{
		Title:   stringFrom(pr["title"]),
		Body:    stringFrom(pr["body"]),
		Number:  intFrom(pr["number"]),
		BaseRef: stringFrom(nestedMap(pr, "base", "ref")),
		HeadRef: stringFrom(nestedMap(pr, "head", "ref")),
		HTMLURL: stringFrom(pr["html_url"]),
		User:    stringFrom(nestedMap(pr, "user", "login")),
	}
}

func nestedMap(m map[string]any, keys ...string) any {
	current := any(m)
	for _, key := range keys {
		nextMap, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = nextMap[key]
	}
	return current
}

func stringFrom(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	default:
		return fmt.Sprintf("%v", val)
	}
}

func intFrom(v any) int {
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	default:
		return 0
	}
}

func buildUserPrompt(files []filePayload, pr prMeta) (string, error) {
	payload := map[string]any{
		"changed_files": files,
		"pull_request":  pr,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func openAIRequest(systemPrompt, userPrompt, model string) (string, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return "", errors.New("OPENAI_API_KEY is not set")
	}

	body := map[string]any{
		"model":       model,
		"temperature": 0.2,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"response_format": map[string]string{"type": "json_object"},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4_096))
		return "", fmt.Errorf("openai request failed: %s: %s", resp.Status, string(bodyBytes))
	}

	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", err
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("openai response missing choices")
	}
	return decoded.Choices[0].Message.Content, nil
}

func parseReviewResult(raw string) (reviewResult, error) {
	var result reviewResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return reviewResult{}, err
	}
	return result, nil
}

func postReviewComment(result reviewResult) error {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return errors.New("GITHUB_TOKEN is not set")
	}
	eventPath := os.Getenv("GITHUB_EVENT_PATH")
	if eventPath == "" {
		return errors.New("GITHUB_EVENT_PATH is not set")
	}

	data, err := os.ReadFile(eventPath)
	if err != nil {
		return err
	}

	var event struct {
		PullRequest struct {
			Links struct {
				Comments struct {
					Href string `json:"href"`
				} `json:"comments"`
			} `json:"_links"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return err
	}
	commentsURL := event.PullRequest.Links.Comments.Href
	if commentsURL == "" {
		return errors.New("comments URL not found in event payload")
	}

	body := buildReviewBody(result)
	payload := map[string]string{"body": body}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, commentsURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: reviewCommentTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4_096))
		return fmt.Errorf("failed to post review comment: %s: %s", resp.Status, string(bodyBytes))
	}

	return nil
}

func buildReviewBody(result reviewResult) string {
	var b strings.Builder
	b.WriteString("### 🤖 AI Code Review – Summary\n\n")
	b.WriteString(result.Summary)
	if len(result.RepoSuggestions) > 0 {
		b.WriteString("\n\n**Repository/CI suggestions:**\n")
		for _, suggestion := range result.RepoSuggestions {
			b.WriteString("- ")
			b.WriteString(suggestion)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func postInlineComments(findings []finding) error {
	if len(findings) == 0 {
		return nil
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return errors.New("GITHUB_TOKEN is not set")
	}
	eventPath := os.Getenv("GITHUB_EVENT_PATH")
	if eventPath == "" {
		return errors.New("GITHUB_EVENT_PATH is not set")
	}

	data, err := os.ReadFile(eventPath)
	if err != nil {
		return err
	}

	var event struct {
		PullRequest struct {
			Number int    `json:"number"`
			URL    string `json:"url"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return err
	}
	if event.PullRequest.Number == 0 || event.PullRequest.URL == "" {
		return errors.New("not a pull request event")
	}

	prURL, err := url.Parse(event.PullRequest.URL)
	if err != nil {
		return fmt.Errorf("invalid pull request URL: %w", err)
	}
	segments := strings.Split(strings.Trim(prURL.Path, "/"), "/")
	if len(segments) < 3 { // expect repos/{owner}/{repo}/pulls/{number}
		return errors.New("unexpected pull request URL format")
	}
	basePath := strings.Join(segments[:3], "/")
	reviewsURL := fmt.Sprintf("%s://%s/%s/pulls/%d/reviews", prURL.Scheme, prURL.Host, basePath, event.PullRequest.Number)

	type comment struct {
		Path      string `json:"path"`
		Body      string `json:"body"`
		Side      string `json:"side"`
		StartLine int    `json:"start_line"`
		Line      int    `json:"line"`
	}

	var comments []comment
	for _, f := range findings {
		if f.File == "" || f.StartLine == 0 || f.Details == "" {
			continue
		}
		title := f.Title
		if title == "" {
			title = "Suggestion"
		}
		body := fmt.Sprintf("**%s** (%s)\n\n%s\n\n", title, f.Priority, f.Details)
		if f.SuggestedPatch != "" {
			body += "```diff\n" + f.SuggestedPatch + "\n```"
		}
		line := f.EndLine
		if line == 0 {
			line = f.StartLine
		}
		comments = append(comments, comment{
			Path:      f.File,
			Body:      body,
			Side:      "RIGHT",
			StartLine: f.StartLine,
			Line:      line,
		})
	}

	if len(comments) == 0 {
		return nil
	}

	payload := map[string]any{
		"event":    "COMMENT",
		"comments": comments,
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, reviewsURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: reviewCommentTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4_096))
		return fmt.Errorf("failed to post inline comments: %s: %s", resp.Status, string(bodyBytes))
	}
	return nil
}
