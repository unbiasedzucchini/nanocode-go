package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ANSI colors
const (
	RESET = "\033[0m"
	BOLD  = "\033[1m"
	DIM   = "\033[2m"
	BLUE  = "\033[34m"
	CYAN  = "\033[36m"
	GREEN = "\033[32m"
	RED   = "\033[31m"
)

const apiURL = "https://openrouter.ai/api/v1/messages"

// --- API types ---

type config struct {
	APIKey       string
	Model        string
	SystemPrompt string
}

type apiRequest struct {
	Model    string           `json:"model"`
	MaxToks  int              `json:"max_tokens"`
	System   string           `json:"system"`
	Messages []message        `json:"messages"`
	Tools    []toolSchema     `json:"tools"`
}

type message struct {
	Role    string        `json:"role"`
	Content any           `json:"content"` // string or []contentBlock
}

type contentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

type toolResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}

type apiResponse struct {
	Content []contentBlock `json:"content"`
}

type toolSchema struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"input_schema"`
}

type inputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]propertySchema `json:"properties"`
	Required   []string                  `json:"required"`
}

type propertySchema struct {
	Type string `json:"type"`
}

// --- Tool definitions ---

type toolDef struct {
	Schema toolSchema
	Fn     func(map[string]any) string
}

var tools = []toolDef{
	{toolSchema{"read", "Read file with line numbers (file path, not directory)", inputSchema{"object",
		map[string]propertySchema{"path": {"string"}, "offset": {"integer"}, "limit": {"integer"}},
		[]string{"path"}}}, toolRead},
	{toolSchema{"write", "Write content to file", inputSchema{"object",
		map[string]propertySchema{"path": {"string"}, "content": {"string"}},
		[]string{"path", "content"}}}, toolWrite},
	{toolSchema{"edit", "Replace old with new in file (old must be unique unless all=true)", inputSchema{"object",
		map[string]propertySchema{"path": {"string"}, "old": {"string"}, "new": {"string"}, "all": {"boolean"}},
		[]string{"path", "old", "new"}}}, toolEdit},
	{toolSchema{"glob", "Find files by pattern, sorted by mtime", inputSchema{"object",
		map[string]propertySchema{"pat": {"string"}, "path": {"string"}},
		[]string{"pat"}}}, toolGlob},
	{toolSchema{"grep", "Search files for regex pattern", inputSchema{"object",
		map[string]propertySchema{"pat": {"string"}, "path": {"string"}},
		[]string{"pat"}}}, toolGrep},
	{toolSchema{"bash", "Run shell command", inputSchema{"object",
		map[string]propertySchema{"cmd": {"string"}},
		[]string{"cmd"}}}, toolBash},
}

func runTool(name string, args map[string]any) string {
	for _, t := range tools {
		if t.Schema.Name == name {
			return t.Fn(args)
		}
	}
	return fmt.Sprintf("error: unknown tool %s", name)
}

func toolSchemas() []toolSchema {
	s := make([]toolSchema, len(tools))
	for i, t := range tools {
		s[i] = t.Schema
	}
	return s
}

// --- Tool implementations ---

func toolRead(args map[string]any) string {
	path, _ := args["path"].(string)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	offset := intArg(args, "offset", 0)
	limit := intArg(args, "limit", len(lines))
	if offset > len(lines) {
		offset = len(lines)
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}
	var buf strings.Builder
	for i, line := range lines[offset:end] {
		fmt.Fprintf(&buf, "%4d| %s", offset+i+1, line)
	}
	return buf.String()
}

func toolWrite(args map[string]any) string {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return "ok"
}

func toolEdit(args map[string]any) string {
	path, _ := args["path"].(string)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	text := string(data)
	old, _ := args["old"].(string)
	new_, _ := args["new"].(string)
	if !strings.Contains(text, old) {
		return "error: old_string not found"
	}
	count := strings.Count(text, old)
	all, _ := args["all"].(bool)
	if !all && count > 1 {
		return fmt.Sprintf("error: old_string appears %d times, must be unique (use all=true)", count)
	}
	var result string
	if all {
		result = strings.ReplaceAll(text, old, new_)
	} else {
		result = strings.Replace(text, old, new_, 1)
	}
	if err := os.WriteFile(path, []byte(result), 0o644); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return "ok"
}

func toolGlob(args map[string]any) string {
	base := stringArg(args, "path", ".")
	pat, _ := args["pat"].(string)
	matches, _ := filepath.Glob(filepath.Join(base, pat))
	sort.Slice(matches, func(i, j int) bool {
		fi, _ := os.Stat(matches[i])
		fj, _ := os.Stat(matches[j])
		var ti, tj time.Time
		if fi != nil {
			ti = fi.ModTime()
		}
		if fj != nil {
			tj = fj.ModTime()
		}
		return ti.After(tj)
	})
	if len(matches) == 0 {
		return "none"
	}
	return strings.Join(matches, "\n")
}

func toolGrep(args map[string]any) string {
	patStr, _ := args["pat"].(string)
	re, err := regexp.Compile(patStr)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	base := stringArg(args, "path", ".")
	var hits []string
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			if re.MatchString(scanner.Text()) {
				hits = append(hits, fmt.Sprintf("%s:%d:%s", path, lineNum, scanner.Text()))
				if len(hits) >= 50 {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return "none"
	}
	return strings.Join(hits, "\n")
}

func toolBash(args map[string]any) string {
	cmdStr, _ := args["cmd"].(string)
	cmd := exec.Command("bash", "-c", cmdStr)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	var lines []string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Printf("  %s│ %s%s\n", DIM, line, RESET)
		lines = append(lines, line)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		lines = append(lines, "(timed out after 30s)")
	}
	result := strings.Join(lines, "\n")
	if result == "" {
		return "(empty)"
	}
	return result
}

// --- Helpers ---

func intArg(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		if n, ok := v.(float64); ok {
			return int(n)
		}
	}
	return def
}

func stringArg(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return def
}

func loadEnvFile() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"`)
			if os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- API ---

func callAPI(cfg config, messages []message) (apiResponse, error) {
	req := apiRequest{
		Model:    cfg.Model,
		MaxToks:  8192,
		System:   cfg.SystemPrompt,
		Messages: messages,
		Tools:    toolSchemas(),
	}
	data, err := json.Marshal(req)
	if err != nil {
		return apiResponse{}, err
	}
	httpReq, err := http.NewRequest("POST", apiURL, bytes.NewReader(data))
	if err != nil {
		return apiResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return apiResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return apiResponse{}, err
	}
	if resp.StatusCode != 200 {
		return apiResponse{}, fmt.Errorf("API %d: %s", resp.StatusCode, body)
	}
	var result apiResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return apiResponse{}, err
	}
	return result, nil
}

// --- UI ---

func separator() string {
	return DIM + strings.Repeat("─", 80) + RESET
}

var boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

func renderMarkdown(text string) string {
	return boldRe.ReplaceAllString(text, BOLD+"$1"+RESET)
}

// --- Main ---

func main() {
	loadEnvFile()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot get working directory: %v\n", err)
		os.Exit(1)
	}

	cfg := config{
		APIKey:       os.Getenv("OPENROUTER_API_KEY"),
		Model:        envOr("MODEL", "anthropic/claude-sonnet-4"),
		SystemPrompt: fmt.Sprintf("Concise coding assistant. cwd: %s", cwd),
	}
	if cfg.APIKey == "" {
		fmt.Fprintf(os.Stderr, "OPENROUTER_API_KEY not set\n")
		os.Exit(1)
	}

	fmt.Printf("%snanocode%s | %s%s | %s%s\n\n", BOLD, RESET, DIM, cfg.Model, cwd, RESET)

	var messages []message
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Println(separator())
		fmt.Printf("%s%s❯%s ", BOLD, BLUE, RESET)
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		fmt.Println(separator())
		if input == "" {
			continue
		}
		if input == "/q" || input == "exit" {
			break
		}
		if input == "/c" {
			messages = nil
			fmt.Printf("%s⏺ Cleared conversation%s\n", GREEN, RESET)
			continue
		}

		messages = append(messages, message{Role: "user", Content: input})

		// Agentic loop
		for {
			response, err := callAPI(cfg, messages)
			if err != nil {
				fmt.Printf("%s⏺ Error: %v%s\n", RED, err, RESET)
				break
			}

			var results []toolResult
			for _, block := range response.Content {
				switch block.Type {
				case "text":
					fmt.Printf("\n%s⏺%s %s\n", CYAN, RESET, renderMarkdown(block.Text))
				case "tool_use":
					argPreview := ""
					for _, v := range block.Input {
						s := fmt.Sprintf("%v", v)
						if len(s) > 50 {
							s = s[:50]
						}
						argPreview = s
						break
					}
					fmt.Printf("\n%s⏺ %s%s(%s%s%s)\n", GREEN, capitalize(block.Name), RESET, DIM, argPreview, RESET)

					out := runTool(block.Name, block.Input)
					lines := strings.SplitN(out, "\n", 2)
					preview := lines[0]
					if len(preview) > 60 {
						preview = preview[:60] + "..."
					} else if len(lines) > 1 {
						preview += fmt.Sprintf(" ... +%d lines", strings.Count(out, "\n"))
					}
					fmt.Printf("  %s⎿  %s%s\n", DIM, preview, RESET)

					results = append(results, toolResult{
						Type:      "tool_result",
						ToolUseID: block.ID,
						Content:   out,
					})
				}
			}

			messages = append(messages, message{Role: "assistant", Content: response.Content})
			if len(results) == 0 {
				break
			}
			messages = append(messages, message{Role: "user", Content: results})
		}
		fmt.Println()
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

