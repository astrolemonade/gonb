// Package specialcmd handles special (aka. "magic") commands, that come in two flavors:
//
//   - `%<cmd> {...args...}`: Control the environment (variables) and configure gonb.
//   - `!<shell commands>`: Execute shell commands.
//     Similar to the ipython kernel.
//
// In particular `%help` will print out currently available commands.
package specialcmd

import (
	_ "embed"
	"fmt"
	"github.com/janpfeifer/gonb/internal/jpyexec"
	"golang.org/x/exp/slices"
	"os"
	"strings"
	"time"

	. "github.com/janpfeifer/gonb/common"
	"github.com/janpfeifer/gonb/gonbui/protocol"
	"github.com/janpfeifer/gonb/internal/goexec"
	"github.com/janpfeifer/gonb/internal/kernel"
	"github.com/pkg/errors"
	"k8s.io/klog/v2"
)

// MillisecondsWaitForInput is the wait time for a bash script (started with `!` or `!*`
// special commands, when `%with_inputs` or `%with_password` is used) to run, before an
// input is prompted to the Jupyter Notebook.
const MillisecondsWaitForInput = 200

//go:embed help.md
var HelpMessage string

// cellStatus holds temporary status for the execution of the current cell.
type cellStatus struct {
	withInputs, withPassword bool
}

// Parse will check whether the given code to be executed has any special commands.
//
// Any special commands found in the code will be executed (if execute is set to true) and the corresponding lines used
// from the code will be returned in usedLines -- so they can be excluded from other executors (goexec).
//
// If any errors happen, it is returned in err.
func Parse(msg kernel.Message, goExec *goexec.State, execute bool, codeLines []string, usedLines Set[int]) (err error) {
	status := &cellStatus{}

	for lineNum, line := range codeLines {
		if usedLines.Has(lineNum) {
			continue
		}
		if len(line) > 1 && (line[0] == '%' || line[0] == '!') {
			var cmdStr string
			cmdStr = joinLine(codeLines, lineNum, usedLines)
			cmdType := cmdStr[0]
			cmdStr = cmdStr[1:]
			for cmdStr[0] == ' ' {
				cmdStr = cmdStr[1:] // Skip initial space
			}
			if len(cmdStr) == 0 {
				// Skip empty commands.
				continue
			}
			if execute {
				switch cmdType {
				case '%':
					err = execSpecialConfig(msg, goExec, cmdStr, status)
					if err != nil {
						return
					}
				case '!':
					err = execShell(msg, goExec, cmdStr, status)
					if err != nil {
						return
					}

					// Runs AutoTrack, in case go.mod has changed.
					err = goExec.AutoTrack()
					if err != nil {
						klog.Errorf("goExec.AutoTrack failed: %+v", err)
					}
				}
			}
		}
	}
	return
}

// joinLine starts from fromLine and joins consecutive lines if the current line terminates with a `\n`,
// allowing multi-line commands to be issued.
//
// It returns the joined lines with the '\\\n' replaced by a space, and appends the used lines (including
// fromLine) to usedLines.
func joinLine(lines []string, fromLine int, usedLines Set[int]) (cmdStr string) {
	for ; fromLine < len(lines); fromLine++ {
		cmdStr += lines[fromLine]
		usedLines.Insert(fromLine)
		if cmdStr[len(cmdStr)-1] != '\\' {
			return
		}
		cmdStr = cmdStr[:len(cmdStr)-1] + " "
	}
	return
}

// execSpecialConfig executes special configuration commands (that start with "%), except cell commands.
// See [HelpMessage] for details, and [execCellSpecialCmd].
//
// It only returns errors for system errors that will lead to the kernel restart. Syntax errors
// on the command themselves are simply reported back to jupyter and are not returned here.
//
// It supports msg == nil for testing.
func execSpecialConfig(msg kernel.Message, goExec *goexec.State, cmdStr string, status *cellStatus) error {
	_ = goExec
	var content map[string]any
	if msg != nil && msg.ComposedMsg().Content != nil {
		content = msg.ComposedMsg().Content.(map[string]any)
	}
	parts := splitCmd(cmdStr)
	switch parts[0] {

	// Configures how cell will be executed.
	case "%", "main", "args", "test", "%test":
		// Set arguments for execution, allows one to set flags, etc.
		goExec.Args = parts[1:]
		klog.V(2).Infof("Program args to use (%%%s): %+q", parts[0], goExec.Args)
		if parts[0] == "test" {
			goExec.CellIsTest = true
		}
		// %% and %main are also handled specially by goexec, where it starts a main() clause.
	case "wasm", "%wasm":
		if len(parts) > 1 {
			return errors.Errorf("`%%wasm` takes no extra parameters.")
		}
		goExec.CellIsWasm = true
		var err error
		err = goExec.MakeWasmSubdir()
		if err != nil {
			return errors.WithMessagef(err, "failed to prepare `%%wasm`")
		}
		goExec.WasmDivId = UniqueId() // Unique ID for this cell.

	case "widgets":
		return goExec.Comms.InstallWebSocket(msg)

	case "widgets_hb":
		var hb bool
		hb, err := goExec.Comms.SendHeartbeatAndWait(msg, 1*time.Second)
		if err != nil {
			return err
		}
		if hb {
			return kernel.PublishHtml(msg, "Heartbeat pong received back.")
		} else {
			return kernel.PublishHtml(msg, "Timed-out, no heartbeat pong received. Try installing front-end websockets with %widgets ?")
		}

	case "env":
		// Set environment variables.
		if len(parts) == 2 {
			// Adjust parts if one uses `%env KEY=VALUE` format instead.
			if eqPos := strings.Index(parts[1], "="); eqPos > 1 {
				key := parts[1][:eqPos]
				value := parts[1][eqPos+1:]
				parts = []string{parts[0], key, value}
			}
		}
		if len(parts) != 3 {
			return errors.Errorf("`%%env <VAR_NAME> <value>` (or `%%env <VAR_NAME>=<value>`): it takes 2 arguments, the variable name and it's content, but %d were given", len(parts)-1)
		}
		err := os.Setenv(parts[1], parts[2])
		if err != nil {
			return errors.Wrapf(err, "`%%env %q %q` failed", parts[1], parts[2])
		}
		err = kernel.PublishWriteStream(msg, kernel.StreamStdout,
			fmt.Sprintf("Set: %s=%q\n", parts[1], parts[2]))
		if err != nil {
			klog.Errorf("Failed to output: %+v", err)
		}

	case "cd":
		if len(parts) == 1 {
			pwd, _ := os.Getwd()
			_ = kernel.PublishWriteStream(msg, kernel.StreamStdout,
				fmt.Sprintf("Current directory: %q\n", pwd))
		} else if len(parts) > 2 {
			return errors.Errorf("`%%cd [<directory>]`: it takes none or one argument, but %d were given", len(parts)-1)
		} else {
			err := os.Chdir(ReplaceTildeInDir(parts[1]))
			if err != nil {
				return errors.Wrapf(err, "`%%cd %q` failed", parts[1])
			}
			pwd, _ := os.Getwd()
			err = kernel.PublishWriteStream(msg, kernel.StreamStdout,
				fmt.Sprintf("Changed directory to %q\n", pwd))
			if err != nil {
				klog.Errorf("Failed to output: %+v", err)
			}
			err = os.Setenv(protocol.GONB_DIR_ENV, pwd)
			if err != nil {
				klog.Errorf("Failed to set environment variable %q: %+v", protocol.GONB_DIR_ENV, err)
			}
		}

		// Flags for `go build`:
	case "goflags":
		if len(parts) > 1 {
			nonEmptyArgs := slices.DeleteFunc(parts[1:], func(s string) bool { return s == "" })
			goExec.GoBuildFlags = nonEmptyArgs
		}
		err := kernel.PublishWriteStream(msg, kernel.StreamStdout,
			fmt.Sprintf("%%goflags=%q\n", goExec.GoBuildFlags))
		if err != nil {
			klog.Errorf("Failed publishing contents: %+v", err)
		}

		// Automatic `go get` control:
	case "autoget":
		goExec.AutoGet = true
	case "noautoget":
		goExec.AutoGet = false
	case "help":
		//_ = kernel.PublishWriteStream(msg, kernel.StreamStdout, HelpMessage)
		err := kernel.PublishMarkdown(msg, HelpMessage)
		if err != nil {
			klog.Errorf("Failed publishing help contents: %+v", err)
		}

		// Definitions management.
	case "reset":
		if len(parts) == 1 {
			resetDefinitions(msg, goExec)
		} else {
			if len(parts) > 2 || parts[1] != "go.mod" {
				return errors.Errorf("%%reset only take one optional parameter \"go.mod\"")
			}
		}
		return goExec.GoModInit()
	case "ls", "list":
		listDefinitions(msg, goExec)
	case "rm", "remove":
		removeDefinitions(msg, goExec, parts[1:])

		// Input handling.
	case "with_inputs":
		allowInput := content["allow_stdin"].(bool)
		if !allowInput && (status.withInputs || status.withPassword) {
			return errors.Errorf("%%with_inputs not available in this notebook, it doesn't allow input prompting")
		}
		status.withInputs = true
	case "with_password":
		allowInput := content["allow_stdin"].(bool)
		if !allowInput && (status.withInputs || status.withPassword) {
			return errors.Errorf("%%with_password not available in this notebook, it doesn't allow input prompting")
		}
		status.withPassword = true

		// Files that need tracking for `gopls` (for auto-complete and contextual help).
	case "track":
		execTrack(msg, goExec, parts[1:])
	case "untrack":
		execUntrack(msg, goExec, parts[1:])

		// Fix issues with `go work`.
	case "goworkfix":
		return goExec.GoWorkFix(msg)

		// These commands should always be the first ones, and if they are parsed here (as opposed to being processed by specialCells)
		// they were in the middle somewhere.
	case "%writefile", "%scrip", "%bash", "%sh":
		err := kernel.PublishWriteStream(msg, kernel.StreamStderr, fmt.Sprintf("\"%%%s\" can only appear at the start of the cell, it is ignore if in the middle of the cell.", parts[0]))
		if err != nil {
			klog.Errorf("Error while reporting back on message command \"%%%s\" kernel: %+v", parts[0], err)
		}
		return errors.Errorf("\"%%%s\" can only appear at the start of the cell, it is ignore if in the middle of the cell.", parts[0])

	default:
		err := kernel.PublishWriteStream(msg, kernel.StreamStderr, fmt.Sprintf("\"%%%s\" unknown or not implemented yet.", parts[0]))
		if err != nil {
			klog.Errorf("Error while reporting back on unimplemented message command \"%%%s\" kernel: %+v", parts[0], err)
		}
	}
	return nil
}

// execShell executes `cmdStr` properly redirecting outputs to display in hte notebook.
//
// It only returns errors for system errors that will lead to the kernel restart. Syntax errors
// on the command themselves are simply reported back to jupyter and are not returned here.
func execShell(msg kernel.Message, goExec *goexec.State, cmdStr string, status *cellStatus) error {
	var execDir string // Default "", means current directory.
	if cmdStr[0] == '*' {
		cmdStr = cmdStr[1:]
		execDir = goExec.TempDir
	}
	if status.withInputs {
		status.withInputs = false
		status.withPassword = false
		return jpyexec.New(msg, "/bin/bash", "-c", cmdStr).
			ExecutionCount(msg.Kernel().ExecCounter).
			InDir(execDir).WithInputs(MillisecondsWaitForInput).Exec()
	} else if status.withPassword {
		status.withInputs = false
		status.withPassword = false
		return jpyexec.New(msg, "/bin/bash", "-c", cmdStr).
			ExecutionCount(msg.Kernel().ExecCounter).
			InDir(execDir).WithPassword(MillisecondsWaitForInput).Exec()
	} else {
		return jpyexec.New(msg, "/bin/bash", "-c", cmdStr).
			ExecutionCount(msg.Kernel().ExecCounter).
			InDir(execDir).Exec()
	}
}

// splitCmd split the special command into it's parts separated by space(s). It also
// accepts quotes to allow spaces to be included in a part. E.g.: `%args --text "hello world"`
// should be split into ["%args", "--text", "hello world"].
func splitCmd(cmd string) (parts []string) {
	partStarted := false
	inQuotes := false
	part := ""
	for pos := 0; pos < len(cmd); pos++ {
		c := cmd[pos]

		isSpace := c == ' ' || c == '\t' || c == '\n'
		if !inQuotes && isSpace {
			if partStarted {
				parts = append(parts, part)
			}
			part = ""
			partStarted = false
			continue
		}

		isQuote := c == '"'
		if isQuote {
			if inQuotes {
				inQuotes = false
			} else {
				inQuotes = true
				partStarted = true // Allows for empty argument.
			}
			continue
		}

		isEscape := c == '\\'
		// Outside of quotes "\" is only a normal character.
		if isEscape && inQuotes {
			if pos == len(cmd)-1 {
				// Odd last character ... but we don't do anything then.
				break
			}
			pos++
			c = cmd[pos]
			switch c {
			case 'n':
				c = '\n'
			case 't':
				c = '\t'
			default:
				// No effect. But it allows backslash+quote to render a quote within quotes.
			}
		}

		part = fmt.Sprintf("%s%c", part, c)
		partStarted = true
	}
	if partStarted {
		parts = append(parts, part)
	}
	return
}

var (
	CellSpecialCommands = SetWithValues(
		"%%writefile",
		"%%script",
		"%%bash",
		"%%sh")
)

// IsGoCell returns whether the cell is expected to be a Go cell, based on the first line.
//
// The first line may contain special commands that change the interpretation of the cell, e.g.: "%%script", "%%writefile".
func IsGoCell(firstLine string) bool {
	parts := strings.Split(firstLine, " ")
	return CellSpecialCommands.Has(parts[0])
}

// ExecuteSpecialCell checks whether it is a special cell (see [CellSpecialCommands]), and if so it executes the special cell command.
//
// It returns if this was a special cell (if true it executes it), and potentially an execution error, if one happened.
func ExecuteSpecialCell(msg kernel.Message, goExec *goexec.State, lines []string) (isSpecialCell bool, err error) {
	if len(lines) == 0 {
		return
	}
	parts := strings.Split(lines[0], " ")
	if !CellSpecialCommands.Has(parts[0]) {
		return
	}
	isSpecialCell = true
	klog.V(2).Infof("Executing special cell command %q", parts)

	switch parts[0] {
	case "%%writefile":
		args := parts[1:]
		err = cellCmdWritefile(msg, goExec, args, lines[1:])
		return

	default:
		err = errors.Errorf("special cell command %q not implemented", parts[0])
		return
	}
}

// cellCmdWritefile implements `%%writefile`.
func cellCmdWritefile(msg kernel.Message, goExec *goexec.State, args []string, lines []string) error {
	var appendToFile bool
	if len(args) > 1 && args[0] == "-a" {
		appendToFile = true
		args = args[1:]
	}
	if len(args) != 1 {
		return errors.Errorf("expected \"%%%%writefile [-a] <file_name>\", but got %q instead", args)
	}
	filePath := args[0]
	filePath = ReplaceTildeInDir(filePath)
	filePath = ReplaceEnvVars(filePath)
	err := writeLinesToFile(filePath, lines, appendToFile)
	if err != nil {
		return err
	}
	if appendToFile {
		_ = kernel.PublishWriteStream(msg, kernel.StreamStderr, fmt.Sprintf("Cell contents appended to %q.\n", filePath))
	} else {
		_ = kernel.PublishWriteStream(msg, kernel.StreamStderr, fmt.Sprintf("Cell contents written to %q.\n", filePath))
	}
	return nil
}

// writeLinesToFile. If `append` is true open the file with append.
func writeLinesToFile(filePath string, lines []string, appendToFile bool) error {
	var f *os.File
	var err error
	if appendToFile {
		f, err = os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	} else {
		f, err = os.Create(filePath)
	}
	if err != nil {
		return errors.Wrapf(err, "failed to open %q", filePath)
	}
	defer func() { _ = f.Close() }()
	for _, line := range lines {
		_, err = fmt.Fprintln(f, line)
		if err != nil {
			return errors.Wrapf(err, "failed writing to %q", filePath)
		}
	}
	return nil
}
