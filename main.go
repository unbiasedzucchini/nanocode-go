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
	RESET  = "\033[0m"
	BOLD   = "\033[1m"
	DIM    = "\033[2m"
	BLUE   = "\033[34m"
	CYAN   = "\033[36m"
	GREEN  = "\033[32m"
	RED    = "\033[31m"
)

var (
	openrouterKey string
	apiURL        string
	model         string
)

func init() {
	// Load .env from home directory
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
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if os.Getenv(strings.TrimSpace(k)) == "" {
			os.Setenv(strings.TrimSpace(k), v)
		}
	}

	openrouterKey = os.Getenv("OPENROUTER_API_KEY")
	if openrouterKey != "" {
		apiURL = "https://openrouter.ai/api/v1/messages"
		model = envOr("MODEL", "anthropic/claude-sonnet-4")
		return
	}
	
	apiURL = "https://api.anthropic.com/v1/messages"
	model = envOr("MODEL", "claude-sonnet-4")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- Tool implementations ---

func toolRead(args map[string]any) string {
	path, _ := args["path"].(string)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	
	lines := strings.SplitAfter(string(data), "\n")
	// Remove trailing empty element from split
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
	fullPattern := filepath.Join(base, pat)
	matches, _ := filepath.Glob(fullPattern)
	
	// Sort by mtime descending
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
			if !re.MatchString(scanner.Text()) {
				continue
			}
			
			hits = append(hits, fmt.Sprintf("%s:%d:%s", path, lineNum, scanner.Text()))
			if len(hits) >= 50 {
				return fs.SkipAll
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
	cmd.Stderr = nil // merge via pipe
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout
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
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
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

// --- Tool definitions ---

type tool struct {
	Name   string
	Desc   string
	Params []param
	Fn     func(map[string]any) string
}

type param struct {
	Name     string
	Type     string
	Optional bool
}

var tools = []tool{
	{"read", "Read file with line numbers (file path, not directory)",
		[]param{{"path", "string", false}, {"offset", "integer", true}, {"limit", "integer", true}}, toolRead},
	{"write", "Write content to file",
		[]param{{"path", "string", false}, {"content", "string", false}}, toolWrite},
	{"edit", "Replace old with new in file (old must be unique unless all=true)",
		[]param{{"path", "string", false}, {"old", "string", false}, {"new", "string", false}, {"all", "boolean", true}}, toolEdit},
	{"glob", "Find files by pattern, sorted by mtime",
		[]param{{"pat", "string", false}, {"path", "string", true}}, toolGlob},
	{"grep", "Search files for regex pattern",
		[]param{{"pat", "string", false}, {"path", "string", true}}, toolGrep},
	{"bash", "Run shell command",
		[]param{{"cmd", "string", false}}, toolBash},
}

func makeSchema() []any {
	var result []any
	for _, t := range tools {
		props := map[string]any{}
		var required []string
		for _, p := range t.Params {
			props[p.Name] = map[string]any{"type": p.Type}
			if !p.Optional {
				required = append(required, p.Name)
			}
		}
		result = append(result, map[string]any{
			"name":        t.Name,
			"description": t.Desc,
			"input_schema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		})
	}
	return result
}

func runTool(name string, args map[string]any) string {
	for _, t := range tools {
		if t.Name == name {
			return t.Fn(args)
		}
	}
	return fmt.Sprintf("error: unknown tool %s", name)
}

// --- API ---

func callAPI(messages []any, systemPrompt string) (map[string]any, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": 8192,
		"system":     systemPrompt,
		"messages":   messages,
		"tools":      makeSchema(),
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if openrouterKey != "" {
		req.Header.Set("Authorization", "Bearer "+openrouterKey)
	} else {
		req.Header.Set("x-api-key", os.Getenv("ANTHROPIC_API_KEY"))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API %d: %s", resp.StatusCode, string(respBody))
	}
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// --- UI helpers ---

func separator() string {
	return fmt.Sprintf("%s%s%s", DIM, strings.Repeat("─", 80), RESET)
}

var boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

func renderMarkdown(text string) string {
	return boldRe.ReplaceAllString(text, BOLD+"$1"+RESET)
}

// --- Main ---

func processUserInput(scanner *bufio.Scanner, messages *[]any) (string, bool) {
	fmt.Println(separator())
	fmt.Printf("%s%s❯%s ", BOLD, BLUE, RESET)
	
	if !scanner.Scan() {
		return "", false
	}
	
	input := strings.TrimSpace(scanner.Text())
	fmt.Println(separator())
	
	if input == "" {
		return "", true
	}
	
	if input == "/q" || input == "exit" {
		return "", false
	}
	
	if input == "/c" {
		*messages = nil
		fmt.Printf("%s⏺ Cleared conversation%s\n", GREEN, RESET)
		return "", true
	}
	
	*messages = append(*messages, map[string]any{
		"role":    "user",
		"content": input,
	})
	
	return input, true
}

func processContentBlock(block map[string]any, toolResults *[]any) {
	blockType, _ := block["type"].(string)
	
	if blockType == "text" {
		text, _ := block["text"].(string)
		fmt.Printf("\n%s⏺%s %s\n", CYAN, RESET, renderMarkdown(text))
		return
	}
	
	if blockType != "tool_use" {
		return
	}
	
	toolName, _ := block["name"].(string)
	toolArgs, _ := block["input"].(map[string]any)
	toolID, _ := block["id"].(string)
	
	// Preview: first arg value, truncated
	argPreview := ""
	for _, v := range toolArgs {
		s := fmt.Sprintf("%v", v)
		if len(s) > 50 {
			s = s[:50]
		}
		argPreview = s
		break
	}
	fmt.Printf("\n%s⏺ %s%s(%s%s%s)\n", GREEN, strings.Title(toolName), RESET, DIM, argPreview, RESET)
	
	result := runTool(toolName, toolArgs)
	lines := strings.SplitN(result, "\n", 2)
	preview := lines[0]
	
	if len(preview) > 60 {
		preview = preview[:60] + "..."
	} else if len(lines) > 1 {
		allLines := strings.Count(result, "\n")
		preview += fmt.Sprintf(" ... +%d lines", allLines)
	}
	fmt.Printf("  %s⎿  %s%s\n", DIM, preview, RESET)
	
	*toolResults = append(*toolResults, map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolID,
		"content":     result,
	})
}

func runAgenticLoop(messages *[]any, systemPrompt string) {
	for {
		response, err := callAPI(*messages, systemPrompt)
		if err != nil {
			fmt.Printf("%s⏺ Error: %v%s\n", RED, err, RESET)
			break
		}
		
		contentBlocks, _ := response["content"].([]any)
		var toolResults []any
		
		for _, raw := range contentBlocks {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			processContentBlock(block, &toolResults)
		}
		
		*messages = append(*messages, map[string]any{
			"role":    "assistant",
			"content": contentBlocks,
		})
		
		if len(toolResults) == 0 {
			break
		}
		
		*messages = append(*messages, map[string]any{
			"role":    "user",
			"content": toolResults,
		})
	}
}

func main() {
	provider := "Anthropic"
	if openrouterKey != "" {
		provider = "OpenRouter"
	}
	
	cwd, _ := os.Getwd()
	fmt.Printf("%snanocode%s | %s%s (%s) | %s%s\n\n", BOLD, RESET, DIM, model, provider, cwd, RESET)
	
	var messages []any
	systemPrompt := fmt.Sprintf("Concise coding assistant. cwd: %s", cwd)
	scanner := bufio.NewScanner(os.Stdin)
	
	for {
		input, shouldContinue := processUserInput(scanner, &messages)
		if !shouldContinue {
			break
		}
		
		if input == "" {
			continue
		}
		
		runAgenticLoop(&messages, systemPrompt)
		fmt.Println()
	}
}
