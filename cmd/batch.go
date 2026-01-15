// Copyright 2025 Nandor Kis
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

var (
	batchFile string
)

// Task represents a single task extracted from the markdown file
type Task struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

var batchCmd = &cobra.Command{
	Use:   "batch",
	Short: "Create multiple containers from a task file",
	Long: `Analyze a markdown file with multiple tasks and create separate containers for each.

Uses AI to identify distinct tasks in the file, then lets you select which ones
to start as separate Maestro containers.

Examples:
  maestro batch --file tasks.md
  maestro batch -f sprint-backlog.md`,
	RunE: runBatch,
}

func init() {
	rootCmd.AddCommand(batchCmd)
	batchCmd.Flags().StringVarP(&batchFile, "file", "f", "", "Markdown file containing tasks (required)")
	batchCmd.MarkFlagRequired("file")
}

func runBatch(cmd *cobra.Command, args []string) error {
	// Read the markdown file
	content, err := os.ReadFile(batchFile)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	fmt.Println("Analyzing tasks...")

	// Use LLM to analyze and extract tasks
	tasks, err := analyzeTasks(string(content))
	if err != nil {
		return fmt.Errorf("failed to analyze tasks: %w", err)
	}

	if len(tasks) == 0 {
		fmt.Println("No distinct tasks found in the file.")
		return nil
	}

	// Display found tasks
	fmt.Printf("\nFound %d task(s):\n", len(tasks))
	for _, task := range tasks {
		fmt.Printf("  %d. %s\n", task.Number, task.Title)
	}

	// Prompt for selection
	selectedTasks, err := promptTaskSelection(tasks)
	if err != nil {
		return err
	}

	if len(selectedTasks) == 0 {
		fmt.Println("No tasks selected. Exiting.")
		return nil
	}

	fmt.Printf("\nStarting %d container(s)...\n\n", len(selectedTasks))

	// Create containers in parallel, passing full markdown as reference
	if err := createContainersInParallel(selectedTasks, string(content)); err != nil {
		return err
	}

	fmt.Println("\nDone! Use 'maestro list' to see container status.")
	return nil
}

// analyzeTasks uses Claude to analyze the markdown and extract tasks
func analyzeTasks(content string) ([]Task, error) {
	prompt := fmt.Sprintf(`Analyze this document and identify distinct tasks or work items.

Document:
---
%s
---

Instructions:
1. Identify each separate task, feature request, bug fix, or work item
2. Tasks might be indicated by headers, numbered lists, bullet points, or separate sections
3. Extract a short title (max 60 chars) and the full description for each task
4. Number them starting from 1

Respond ONLY with valid JSON in this exact format (no markdown, no explanation):
{"tasks": [{"number": 1, "title": "Short task title", "description": "Full task description..."}, ...]}

If no distinct tasks are found, respond with: {"tasks": []}`, content)

	// Call Claude CLI to analyze tasks (--print is read-only, no permissions needed)
	cmd := exec.Command("claude", "--print")
	cmd.Stdin = strings.NewReader(prompt)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to call Claude: %w\nOutput: %s", err, string(output))
	}

	// Parse JSON response
	outputStr := strings.TrimSpace(string(output))

	// Try to extract JSON if wrapped in markdown code blocks
	if strings.Contains(outputStr, "```") {
		re := regexp.MustCompile("```(?:json)?\\s*([\\s\\S]*?)```")
		if matches := re.FindStringSubmatch(outputStr); len(matches) > 1 {
			outputStr = strings.TrimSpace(matches[1])
		}
	}

	var result struct {
		Tasks []Task `json:"tasks"`
	}

	if err := json.Unmarshal([]byte(outputStr), &result); err != nil {
		// Try to find JSON object in the output
		start := strings.Index(outputStr, "{")
		end := strings.LastIndex(outputStr, "}")
		if start >= 0 && end > start {
			outputStr = outputStr[start : end+1]
			if err := json.Unmarshal([]byte(outputStr), &result); err != nil {
				return nil, fmt.Errorf("failed to parse response: %w\nOutput: %s", err, string(output))
			}
		} else {
			return nil, fmt.Errorf("failed to parse response: %w\nOutput: %s", err, string(output))
		}
	}

	return result.Tasks, nil
}

// promptTaskSelection prompts the user to select which tasks to start
func promptTaskSelection(tasks []Task) ([]Task, error) {
	fmt.Printf("\nWhich tasks to start? ")
	fmt.Printf("[1-%d, 'all', or comma-separated like '1,3,5'] (default: all): ", len(tasks))

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	input = strings.TrimSpace(strings.ToLower(input))

	// Default to all tasks if user just presses Enter
	if input == "" {
		return tasks, nil
	}

	// Handle 'all'
	if input == "all" || input == "a" {
		return tasks, nil
	}

	// Handle range like '1-3'
	if strings.Contains(input, "-") && !strings.Contains(input, ",") {
		parts := strings.Split(input, "-")
		if len(parts) == 2 {
			start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err1 == nil && err2 == nil && start >= 1 && end <= len(tasks) && start <= end {
				var selected []Task
				for i := start - 1; i < end; i++ {
					selected = append(selected, tasks[i])
				}
				return selected, nil
			}
		}
	}

	// Handle comma-separated list like '1,3,5'
	var selected []Task
	for _, part := range strings.Split(input, ",") {
		num, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			fmt.Printf("Warning: '%s' is not a valid number, skipping\n", part)
			continue
		}
		if num < 1 || num > len(tasks) {
			fmt.Printf("Warning: %d is out of range, skipping\n", num)
			continue
		}
		selected = append(selected, tasks[num-1])
	}

	return selected, nil
}

// ContainerResult holds the result of creating a container
type ContainerResult struct {
	TaskNumber int
	TaskTitle  string
	Success    bool
	Message    string
}

// createContainersInParallel creates containers for selected tasks concurrently
func createContainersInParallel(tasks []Task, fullMarkdown string) error {
	var wg sync.WaitGroup
	results := make(chan ContainerResult, len(tasks))

	// Track created containers for summary
	var createdContainers []string
	var mu sync.Mutex

	for _, task := range tasks {
		wg.Add(1)
		go func(t Task) {
			defer wg.Done()

			result := ContainerResult{
				TaskNumber: t.Number,
				TaskTitle:  t.Title,
			}

			// Build the task description with full context
			taskDescription := t.Description
			if taskDescription == "" {
				taskDescription = t.Title
			}

			// Build the full prompt with markdown as reference
			fullPrompt := fmt.Sprintf(`You are working on Task %d: %s

YOUR SPECIFIC TASK:
%s

FULL DOCUMENT FOR REFERENCE:
---
%s
---

Focus ONLY on your assigned task above. The document is provided for context only.`,
				t.Number, t.Title, taskDescription, fullMarkdown)

			// Generate branch name from the specific task (not the full prompt)
			branchName, _, err := generateBranchAndPrompt(taskDescription, false)
			if err != nil {
				result.Success = false
				result.Message = fmt.Sprintf("failed to generate branch: %v", err)
				results <- result
				return
			}

			// Validate branch name
			if !isValidBranchName(branchName) {
				branchName = generateSimpleBranch(t.Title)
			}

			// Get container name
			containerName, err := getNextContainerName(branchName)
			if err != nil {
				result.Success = false
				result.Message = fmt.Sprintf("failed to get container name: %v", err)
				results <- result
				return
			}

			// Create the container (simplified version without auto-connect)
			if err := createBatchContainer(containerName, branchName, fullPrompt); err != nil {
				result.Success = false
				result.Message = fmt.Sprintf("failed to create container: %v", err)
				results <- result
				return
			}

			mu.Lock()
			createdContainers = append(createdContainers, containerName)
			mu.Unlock()

			result.Success = true
			result.Message = containerName
			results <- result
		}(task)
	}

	// Wait for all to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Print results as they come in
	successCount := 0
	for result := range results {
		if result.Success {
			fmt.Printf("  [%d] %s\n", result.TaskNumber, result.Message)
			successCount++
		} else {
			fmt.Printf("  [%d] %s\n", result.TaskNumber, result.Message)
		}
	}

	fmt.Printf("\nCreated %d/%d containers successfully.\n", successCount, len(tasks))
	return nil
}

// createBatchContainer creates a single container without connecting
func createBatchContainer(containerName, branchName, planningPrompt string) error {
	// Step 1: Ensure Docker image
	if err := ensureDockerImage(); err != nil {
		return fmt.Errorf("failed to ensure Docker image: %w", err)
	}

	// Step 2: Start container
	if err := startContainer(containerName); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Step 3: Copy project files
	if err := copyProjectToContainer(containerName); err != nil {
		return fmt.Errorf("failed to copy project: %w", err)
	}

	// Step 4: Copy additional folders
	if err := copyAdditionalFolders(containerName); err != nil {
		return fmt.Errorf("failed to copy additional folders: %w", err)
	}

	// Step 5: Initialize git branch
	if err := initializeGitBranch(containerName, branchName); err != nil {
		return fmt.Errorf("failed to initialize git branch: %w", err)
	}

	// Step 6: Configure git user
	if err := configureGitUser(containerName); err != nil {
		// Just warn, don't fail
	}

	// Step 7: Setup GitHub remote
	if err := setupGitHubRemote(containerName); err != nil {
		// Just warn, don't fail
	}

	// Step 8: Start tmux session
	if err := startTmuxSession(containerName, branchName, planningPrompt, false); err != nil {
		return fmt.Errorf("failed to start tmux session: %w", err)
	}

	return nil
}
