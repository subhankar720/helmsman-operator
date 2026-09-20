package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// scaffoldFlags holds all the flags for the scaffold command.
type scaffoldFlags struct {
	appName      string
	owningTeam   string
	costCenterId string
	imageRepo    string
	imageTag     string
	port         int32
	oidcEnabled  bool
	fleetRepo    string
	githubToken  string
	dryRun       bool
}

func newScaffoldCmd() *cobra.Command {
	f := &scaffoldFlags{}

	cmd := &cobra.Command{
		Use:   "scaffold",
		Short: "Scaffold a new application on the Helmsman platform",
		Long: `Scaffold generates an AppDeployment CR and opens a PR in the fleet repo.

Example:
  helmsman-cli scaffold \
    --app-name payments-api \
    --team payments-team \
    --cost-center 100042 \
    --image-repo ghcr.io/myorg/payments-api \
    --image-tag latest \
    --port 8080 \
    --oidc`,

		RunE: func(cmd *cobra.Command, args []string) error {
			return runScaffold(f)
		},
	}

	// Required flags
	cmd.Flags().StringVar(&f.appName, "app-name", "", "Application name (used as resource prefix) [required]")
	cmd.Flags().StringVar(&f.owningTeam, "team", "", "Owning team name [required]")
	cmd.Flags().StringVar(&f.imageRepo, "image-repo", "", "Container image repository (e.g. ghcr.io/org/app) [required]")
	_ = cmd.MarkFlagRequired("app-name")
	_ = cmd.MarkFlagRequired("team")
	_ = cmd.MarkFlagRequired("image-repo")

	// Optional flags with sensible defaults
	cmd.Flags().StringVar(&f.costCenterId, "cost-center", "000000", "Cost centre ID for billing allocation")
	cmd.Flags().StringVar(&f.imageTag, "image-tag", "latest", "Image tag to deploy")
	cmd.Flags().Int32Var(&f.port, "port", 8080, "Port the application listens on")
	cmd.Flags().BoolVar(&f.oidcEnabled, "oidc", false, "Enable OIDC authentication gating via oauth2-proxy")
	cmd.Flags().StringVar(&f.fleetRepo, "fleet-repo", "subhankar720/helmsman-fleet", "GitHub org/repo of the fleet repository")
	cmd.Flags().StringVar(&f.githubToken, "github-token", os.Getenv("GITHUB_TOKEN"), "GitHub PAT with fleet repo write access (or set GITHUB_TOKEN env var)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the AppDeployment CR YAML without opening a PR")

	return cmd
}

// runScaffold is the main logic for the scaffold command.
func runScaffold(f *scaffoldFlags) error {
	// Validate
	if !f.dryRun && f.githubToken == "" {
		return fmt.Errorf("--github-token or GITHUB_TOKEN env var is required (unless --dry-run)")
	}

	fmt.Printf("🔨 Scaffolding application: %s\n", f.appName)

	// ── Step 1: Generate the AppDeployment CR ─────────────────────────────
	cr, err := generateAppDeploymentCR(f)
	if err != nil {
		return fmt.Errorf("failed to generate AppDeployment CR: %w", err)
	}

	fmt.Println("\n📄 Generated AppDeployment CR:")
	fmt.Println("────────────────────────────────────────")
	fmt.Println(cr)
	fmt.Println("────────────────────────────────────────")

	if f.dryRun {
		fmt.Println("\n✅ Dry run complete. No PR opened.")
		return nil
	}

	// ── Step 2: Open a PR in the fleet repo ───────────────────────────────
	fmt.Printf("\n🚀 Opening PR in %s...\n", f.fleetRepo)
	prURL, err := openFleetRepoPR(f, cr)
	if err != nil {
		return fmt.Errorf("failed to open fleet repo PR: %w", err)
	}

	fmt.Printf("\n✅ Done! Review and merge the PR to deploy %s:\n", f.appName)
	fmt.Printf("   %s\n", prURL)
	return nil
}

// generateAppDeploymentCR builds the AppDeployment CR YAML string.
func generateAppDeploymentCR(f *scaffoldFlags) (string, error) {
	// Build as a nested map so yaml.Marshal produces clean, readable YAML
	cr := map[string]interface{}{
		"apiVersion": "platform.helmsman.dev/v1alpha1",
		"kind":       "AppDeployment",
		"metadata": map[string]interface{}{
			"name":      f.appName,
			"namespace": "sample-app", // platform team manages namespace assignment
		},
		"spec": map[string]interface{}{
			"appName":      f.appName,
			"owningTeam":   f.owningTeam,
			"costCenterId": f.costCenterId,
			"image": map[string]interface{}{
				"repository": f.imageRepo,
				"tag":        f.imageTag,
				"pullPolicy": "IfNotPresent",
			},
			"container": map[string]interface{}{
				"port": f.port,
			},
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{
					"cpu":    "100m",
					"memory": "128Mi",
				},
				"limits": map[string]interface{}{
					"cpu":    "500m",
					"memory": "256Mi",
				},
			},
			"replicas": 1,
			"oidc": map[string]interface{}{
				"enabled": f.oidcEnabled,
			},
		},
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cr); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// openFleetRepoPR clones the fleet repo, writes the CR, and opens a PR via GitHub API.
func openFleetRepoPR(f *scaffoldFlags, cr string) (string, error) {
	timestamp := time.Now().Format("20060102-150405")
	branch := fmt.Sprintf("scaffold/%s-%s", f.appName, timestamp)
	filePath := fmt.Sprintf("appdeployments/%s.yaml", f.appName)

	// ── Clone fleet repo into a temp directory ────────────────────────────
	tmpDir, err := os.MkdirTemp("", "helmsman-fleet-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	repoURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", f.githubToken, f.fleetRepo)
	if err := gitRun(tmpDir, "clone", "--depth=1", repoURL, "."); err != nil {
		return "", fmt.Errorf("failed to clone fleet repo: %w", err)
	}

	// ── Configure git identity ────────────────────────────────────────────
	_ = gitRun(tmpDir, "config", "user.email", "helmsman-cli@helmsman.dev")
	_ = gitRun(tmpDir, "config", "user.name", "helmsman-cli")

	// ── Create branch ────────────────────────────────────────────────────
	if err := gitRun(tmpDir, "checkout", "-b", branch); err != nil {
		return "", fmt.Errorf("failed to create branch: %w", err)
	}

	// ── Write the AppDeployment CR file ───────────────────────────────────
	fullPath := filepath.Join(tmpDir, filePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return "", err
	}

	header := fmt.Sprintf("# Generated by helmsman-cli scaffold\n# App: %s | Team: %s | %s\n---\n",
		f.appName, f.owningTeam, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(fullPath, []byte(header+cr), 0644); err != nil {
		return "", fmt.Errorf("failed to write CR file: %w", err)
	}

	// ── Commit and push ───────────────────────────────────────────────────
	if err := gitRun(tmpDir, "add", filePath); err != nil {
		return "", err
	}

	commitMsg := fmt.Sprintf("feat: scaffold %s for %s\n\nGenerated by helmsman-cli scaffold.\nTeam: %s\nImage: %s:%s\nOIDC: %v",
		f.appName, f.owningTeam, f.owningTeam, f.imageRepo, f.imageTag, f.oidcEnabled)

	if err := gitRun(tmpDir, "commit", "-m", commitMsg); err != nil {
		return "", fmt.Errorf("failed to commit: %w", err)
	}

	if err := gitRun(tmpDir, "push", "origin", branch); err != nil {
		return "", fmt.Errorf("failed to push branch: %w", err)
	}

	// ── Open PR via GitHub API ────────────────────────────────────────────
	prURL, err := createGitHubPR(f, branch)
	if err != nil {
		return "", fmt.Errorf("failed to create PR: %w", err)
	}

	return prURL, nil
}

// createGitHubPR calls the GitHub REST API to open a pull request.
func createGitHubPR(f *scaffoldFlags, branch string) (string, error) {
	oidcStatus := "disabled"
	if f.oidcEnabled {
		oidcStatus = "enabled ✅"
	}

	body := fmt.Sprintf(`## New Application: %s

**Scaffolded by:** helmsman-cli
**Team:** %s
**Cost Centre:** %s
**Image:** %s:%s
**Port:** %d
**OIDC:** %s

### What this PR does
Adds an `+"`AppDeployment`"+` CR to the fleet repo. Once merged, Argo CD will sync
the CR to the spoke cluster and the Helmsman operator will:
- Register a Keycloak OIDC client (if OIDC enabled)
- Write credentials to Vault
- Create StatefulSet, Services, NetworkPolicies, ExternalSecret
- Set status to READY: True

### Review checklist
- [ ] App name is correct and follows naming conventions
- [ ] Team and cost centre are valid
- [ ] Image repository exists and is accessible from GHCR
- [ ] OIDC requirement is correct for this application
- [ ] Resources (CPU/memory) are appropriate for the workload`,
		f.appName, f.owningTeam, f.costCenterId,
		f.imageRepo, f.imageTag, f.port, oidcStatus)

	payload := map[string]interface{}{
		"title": fmt.Sprintf("feat: scaffold %s for %s", f.appName, f.owningTeam),
		"body":  body,
		"head":  branch,
		"base":  "main",
	}

	payloadBytes, _ := json.Marshal(payload)
	parts := strings.SplitN(f.fleetRepo, "/", 2)
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", parts[0], parts[1])

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+f.githubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		HTMLURL string `json:"html_url"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode GitHub API response: %w", err)
	}
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, result.Message)
	}

	return result.HTMLURL, nil
}

// gitRun runs a git command in the given directory.
func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
