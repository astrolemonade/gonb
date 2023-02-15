package goexec

import (
	"context"
	"fmt"
	"github.com/janpfeifer/gonb/gonbui/protocol"
	"github.com/janpfeifer/gonb/kernel"
	"github.com/pkg/errors"
)

// This file implements inspecting an identifier in a Cell and auto-complete functionalities.

// InspectIdentifierInCell implements an `inspect_request` from Jupyter, using `gopls`.
// It updates `main.go` with the cell contents (given as lines)
func (s *State) InspectIdentifierInCell(lines []string, skipLines map[int]bool, cursorLine, cursorCol int) (kernel.MIMEMap, error) {
	if s.gopls == nil {
		// gopls not installed.
		return make(kernel.MIMEMap), nil
	}
	if skipLines[cursorLine] {
		// Only Go code can be inspected here.
		return nil, errors.Errorf("goexec.InspectIdentifierInCell() can only inspect Go code, line %d is a secial command line: %q", cursorLine, lines[cursorLine])
	}

	// Generate `main.go` with contents of current cell.
	cursorInCell := Cursor{cursorLine, cursorCol}
	_, cursorInFile, err := s.parseLinesAndComposeMain(lines, skipLines, cursorInCell)
	if err != nil {
		if errors.Is(err, ParseError) || errors.Is(err, CursorLost) {
			return make(kernel.MIMEMap), nil
		}
	}

	// Query `gopls`.
	ctx := context.Background()
	var desc string
	s.gopls.ResetFile(s.MainPath())
	desc, err = s.gopls.Definition(ctx, s.MainPath(), cursorInFile.Line, cursorInFile.Col)
	if err != nil {
		return nil, errors.Cause(err)
	}

	// Return MIMEMap with markdown.
	return kernel.MIMEMap{protocol.MIMETextMarkdown: desc}, nil
}

var (
	ParseError = fmt.Errorf("failed to parse cell contents")
	CursorLost = fmt.Errorf("cursor position not rendered in main.go")
)
