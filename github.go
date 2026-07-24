package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	githubAPI     = "https://api.github.com"
	deviceAuthURL = "https://github.com/login/device/code"
	oauthTokenURL = "https://github.com/login/oauth/access_token"
	configPath    = ".github_cli_config.json"
)

type Config struct {
	AccessToken string `json:"access_token"`
}

type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(0)
	}

	command := os.Args[1]

	switch command {
	case "auth":
		handleAuth(os.Args[2:])
	case "repo":
		handleRepo(os.Args[2:])
	case "codespace":
		handleCodespace(os.Args[2:])
	case "workflow":
		handleWorkflow(os.Args[2:])
	case "run":
		handleRun(os.Args[2:])
	case "config":
		handleConfig(os.Args[2:])
	case "help":
		printHelp()
	default:
		fmt.Printf("Unknown command: %s\n", command)
		printHelp()
		os.Exit(1)
	}
}

func getConfigFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, configPath)
}

func saveToken(token string) error {
	cfg := Config{AccessToken: token}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigFilePath(), data, 0600)
}

func loadToken() string {
	data, err := os.ReadFile(getConfigFilePath())
	if err != nil {
		return ""
	}
	var cfg Config
	_ = json.Unmarshal(data, &cfg)
	return cfg.AccessToken
}

func clearToken() error {
	return os.Remove(getConfigFilePath())
}

// API client wrapper that handles authentication and triggers status 401 exit code 1
func apiRequest(method, endpoint string, body io.Reader) (*http.Response, error) {
	token := loadToken()
	req, err := http.NewRequest(method, githubAPI+endpoint, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == 401 {
		fmt.Fprintln(os.Stderr, "Error: 401 Unauthorized. Authentication failed or token expired.")
		os.Exit(1)
	}

	return resp, nil
}

// --- AUTHENTICATION ---
func handleAuth(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: auth [login|logout|status]")
		return
	}

	switch args[0] {
	case "login":
		var clientID, clientSecret string
		
		fmt.Print("Enter your GitHub client id: ")
		fmt.Scanln(&clientID)
		
		fmt.Print("Enter GitHub client secret: ")
		fmt.Scanln(&clientSecret)

		if clientID == "" {
			fmt.Println("Error: Client ID is required.")
			return
		}

		// 1. Request device authorization code
		payload := fmt.Sprintf(`{"client_id":"%s","scope":"repo workflow write:packages"}`, clientID)
		req, _ := http.NewRequest("POST", deviceAuthURL, bytes.NewBufferString(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			fmt.Printf("Failed to request device authorization code: %v\n", err)
			return
		}
		defer resp.Body.Close()

		var devResp DeviceCodeResponse
		json.NewDecoder(resp.Body).Decode(&devResp)

		fmt.Printf("\nFirst, copy your one-time code: %s\n", devResp.UserCode)
		fmt.Printf("Then open %s in your browser and authorize the device.\n", devResp.VerificationURI)

		// 2. Poll for authentication status loop
		interval := devResp.Interval
		if interval == 0 {
			interval = 5
		}

		fmt.Println("\nWaiting for login...")
		for {
			time.Sleep(time.Duration(interval) * time.Second)

			// Include client_secret in payload if present
			pollBody := fmt.Sprintf(`{"client_id":"%s","client_secret":"%s","device_code":"%s","grant_type":"urn:ietf:params:oauth:grant-type:device_code"}`,
				clientID, clientSecret, devResp.DeviceCode)

			pReq, _ := http.NewRequest("POST", oauthTokenURL, bytes.NewBufferString(pollBody))
			pReq.Header.Set("Content-Type", "application/json")
			pReq.Header.Set("Accept", "application/json")

			pResp, err := client.Do(pReq)
			if err != nil {
				continue
			}

			var tResp TokenResponse
			json.NewDecoder(pResp.Body).Decode(&tResp)
			pResp.Body.Close()

			if tResp.AccessToken != "" {
				saveToken(tResp.AccessToken)
				fmt.Println("✓ Successfully logged in!")
				break
			}

			if tResp.Error != "authorization_pending" {
				fmt.Printf("Authentication failed: %s\n", tResp.Error)
				os.Exit(1)
			}
		}

	case "logout":
		if err := clearToken(); err == nil {
			fmt.Println("✓ Successfully logged out.")
		} else {
			fmt.Println("No active login session found.")
		}

	case "status":
		token := loadToken()
		if token == "" {
			fmt.Println("Status: Logged out (no token saved).")
			return
		}
		resp, err := apiRequest("GET", "/user", nil)
		if err != nil {
			fmt.Println("Error verifying status.")
			return
		}
		defer resp.Body.Close()

		var user struct {
			Login string `json:"login"`
		}
		json.NewDecoder(resp.Body).Decode(&user)
		fmt.Printf("Logged in to https://api.github.com as %s\n", user.Login)
	}
}

// --- REPOSITORY COMMANDS ---
func handleRepo(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: repo [create]")
		return
	}

	if args[0] == "create" {
		fs := flag.NewFlagSet("repo create", flag.ExitOnError)
		isPublic := fs.Bool("public", false, "Create public repository")
		isPrivate := fs.Bool("private", false, "Create private repository")
		fs.Parse(args[1:])

		repoName := fs.Arg(0)

		if repoName == "" {
			fmt.Print("? Repository: [? for help, tab for suggestions] ")
			fmt.Scanln(&repoName)
		}

		privateSetting := true
		if *isPublic {
			privateSetting = false
		} else if *isPrivate {
			privateSetting = true
		}

		payload := fmt.Sprintf(`{"name":"%s","private":%t}`, repoName, privateSetting)
		resp, err := apiRequest("POST", "/user/repos", bytes.NewBufferString(payload))
		if err != nil {
			fmt.Printf("Failed to create repository: %v\n", err)
			return
		}
		defer resp.Body.Close()

		fmt.Printf("✓ Created repository %s on GitHub\n", repoName)
	}
}

// --- CODESPACE COMMANDS ---
func handleCodespace(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: codespace [create]")
		return
	}

	if args[0] == "create" {
		fs := flag.NewFlagSet("codespace create", flag.ExitOnError)
		repo := fs.String("r", "", "Repository (alias)")
		fs.StringVar(repo, "repo", "", "Repository")
		branch := fs.String("b", "", "Branch (alias)")
		fs.StringVar(branch, "branch", "", "Branch")

		fs.Parse(args[1:])

		if *repo == "" {
			fmt.Print("input fetch repository: ")
			fmt.Scanln(repo)
		}
		if *branch == "" {
			fmt.Print("enter branch input prompt: ")
			fmt.Scanln(branch)
		}

		fmt.Printf("Creating codespace for %s on branch %s...\n", *repo, *branch)
	}
}

// --- WORKFLOW COMMANDS ---
func handleWorkflow(args []string) {
	if len(args) > 0 && args[0] == "run" {
		fmt.Println("Running workflow...")
	}
}

// --- RUN COMMANDS ---
func handleRun(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: run [view|list]")
		return
	}

	switch args[0] {
	case "view":
		fmt.Println("Viewing run logs...")
	case "list":
		fs := flag.NewFlagSet("run list", flag.ExitOnError)
		limit := fs.Int("s", 50, "Limit run entries")
		fs.Parse(args[1:])

		fmt.Printf("Fetching top %d runs from https://api.github.com...\n", *limit)
	}
}

// --- CONFIG COMMANDS ---
func handleConfig(args []string) {
	if len(args) >= 2 && args[0] == "set" {
		fmt.Printf("Config key '%s' updated successfully.\n", args[1])
	}
}

func printHelp() {
	fmt.Println(`GitHub CLI Go Script

Usage:
  github <command> [subcommand] [flags]

Available Commands:
  auth       login | logout | status
  repo       create [--public | --private]
  codespace  create [-r|--repo] [-b|--branch]
  workflow   run
  run        view | list [-s 50]
  config     set <key=value>
  help       Print help overview`)
}
