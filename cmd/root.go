package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
	"github.com/sashabaranov/go-openai"
	"github.com/spf13/cobra"
)

var VERSION = "dev"

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "duckgpt",
	Short: "Ask questions, let GPT do the SQL.",
	Long: `DuckGPT lets you ask questions about your JSON and CSV files.
It uses OpenAI GPT to compose SQL queries and DuckDB to execute them.
Please keep in mind that this will incur costs on your OpenAI account.
`,
	Args:          cobra.NoArgs,
	SilenceErrors: true,
	Version:       VERSION,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		inputReader := bufio.NewReader(os.Stdin)

		assistantInstructions := `You can respond with "SCHEMA <table_name>" to show the schema of a table. You can respond with "QUERY <query>" to execute the provided query and finish the exchange. Only respond with those predefined commands. Read the schema of any tables you want to use first. Always include the file extension in the table names, so table.json or table.csv, and then always alias the table with a custom name. Only send one command per message.`

		var messages []openai.ChatCompletionMessage
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    "system",
			Content: `You are a system that parses natural language data processing queries and constructs a SQL query to answer the question. You can only use pre-specified commands. You may not use natural language in your responses.` + "\n" + assistantInstructions,
		})

		color.Green("What would you like to compute?")
		query, err := inputReader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("could not read query: %w", err)
		}
		query = strings.TrimSpace(query)

		jsonFiles, err := filepath.Glob("*.json")
		if err != nil {
			return fmt.Errorf("could not list json files: %w", err)
		}
		csvFiles, err := filepath.Glob("*.csv")
		if err != nil {
			return fmt.Errorf("could not list csv files: %w", err)
		}

		messages = append(messages, openai.ChatCompletionMessage{
			Role: "user",
			Content: fmt.Sprintf(`User question: %s

Respond with "SCHEMA <table_name>" to show the schema of a table. Respond with "QUERY <query>" to execute the provided query and finish the exchange. The user will respond to these commands.

Example commands:
"SCHEMA hello.json"
"SCHEMA papayas.csv"
"QUERY SELECT SUM(i) FROM numbers.json as numbers GROUP BY true"
"QUERY SELECT papaya_trees.region, AVG(papayas.size) FROM papaya_trees.json as papaya_trees JOIN papayas.csv as papayas ON papaya_tree_id = papaya_trees.id GROUP BY papaya_trees.region"

Never guess the schema of a table. Always ask for it. Only use the above commands, never respond with natural language. Do not end your commands with a period. You can use json and csv files.

Never respond with natural language.

Available tables: %s

%s`, query, strings.Join(append(jsonFiles, csvFiles...), ", "), assistantInstructions),
		})

		openaiToken := os.Getenv("GPTSQL_TOKEN")
		if openaiToken == "" {
			return fmt.Errorf("please provide your OpenAI platform token in the GPTSQL_TOKEN environment variable")
		}
		openaiCli := openai.NewClient(openaiToken)

		for i := 0; i < messageLimit; i++ {
			if debug {
				color.Yellow("DEBUG User Msg: %s", messages[len(messages)-1].Content)
			}
			res, err := openaiCli.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
				Model:       openai.GPT3Dot5Turbo,
				Messages:    messages,
				MaxTokens:   512,
				Temperature: 0.7,
				TopP:        1,
			})
			if err != nil {
				return fmt.Errorf("could not create chat completion: %w", err)
			}
			msg := res.Choices[0].Message
			messages = append(messages, msg)
			body := msg.Content
			body = strings.TrimRight(body, ".;")
			if debug {
				color.Cyan("DEBUG Assistant Msg: %s", body)
			}
			switch {
			case strings.HasPrefix(body, "SCHEMA "):
				if index := strings.Index(body, "\n"); index != -1 {
					body = body[:index]
				}
				tableName := strings.TrimPrefix(body, "SCHEMA ")
				var originalOutput bytes.Buffer
				cmd := exec.Command("duckdb", "-json", "-c", fmt.Sprintf("DESCRIBE SELECT * FROM %s", tableName))
				cmd.Stdout = &originalOutput
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return fmt.Errorf("could not run duckdb to describe table: %w", err)
				}
				var fields []struct {
					ColumnName string `json:"column_name"`
					ColumnType string `json:"column_type"`
				}
				if err := json.Unmarshal(originalOutput.Bytes(), &fields); err != nil {
					return fmt.Errorf("could not unmarshal duckdb describe output: %w", err)
				}
				modifiedOutput, err := json.Marshal(fields)
				if err != nil {
					return fmt.Errorf("could not marshal duckdb describe output: %w", err)
				}
				messages = append(messages, openai.ChatCompletionMessage{
					Role:    "user",
					Content: string(modifiedOutput) + "\n" + assistantInstructions,
				})
			case strings.HasPrefix(body, "QUERY "):
				color.Green("Running query: %s", body)
				query := strings.TrimPrefix(body, "QUERY ")
				cmd := exec.Command("duckdb", "-c", query)
				cmd.Stdout = os.Stdout
				var errBuffer bytes.Buffer
				cmd.Stderr = io.MultiWriter(&errBuffer, os.Stdout)
				if err := cmd.Run(); err != nil {
					messages = append(messages, openai.ChatCompletionMessage{
						Role:    "user",
						Content: errBuffer.String() + "\n" + "Please retry." + "\n" + assistantInstructions,
					})
					continue
				}
				color.Green("\nIs this satisfactory? If yes, please exit (or type 'exit'). If no, please specify the issue.")
				line, err := inputReader.ReadString('\n')
				if err != nil {
					if err == io.EOF {
						return nil
					}
					return fmt.Errorf("could not read input: %w", err)
				}
				line = strings.TrimSpace(line)
				if line == "exit" {
					return nil
				}
				messages = append(messages, openai.ChatCompletionMessage{
					Role:    "user",
					Content: "Please retry with this additional constraint: " + line + "\n" + assistantInstructions + "\n" + "Do not apologize.",
				})

			default:
				return fmt.Errorf("invalid assistant command: %s", body)
			}
		}

		return fmt.Errorf("message limit reached")
	},
}

func Execute(ctx context.Context) {
	cobra.CheckErr(rootCmd.ExecuteContext(ctx))
}

var debug bool
var messageLimit int

func init() {
	rootCmd.Flags().BoolVar(&debug, "debug", false, "Print interactions between DuckGPT and ChatGPT.")
	rootCmd.Flags().IntVar(&messageLimit, "message-limit", 16, "The maximum number of messages to exchange with ChatGPT. Might also be limited by overall conversation size.")
}
