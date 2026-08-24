// Command jira-cli reads Jira Cloud through a stable agent-oriented contract.
package main

import (
	"os"

	"github.com/abigotado/jira-cli/internal/cli"
	"github.com/abigotado/jira-cli/internal/errx"
	"github.com/abigotado/jira-cli/internal/output"
)

func main() {
	os.Exit(int(run()))
}

func run() (code errx.Code) {
	defer func() {
		if recover() != nil {
			writer := output.New(output.FormatJSON, nil)
			code = writer.Failure(errx.Internal("jira-cli stopped after an unexpected internal failure"))
		}
	}()
	return cli.Execute(os.Args[1:])
}
