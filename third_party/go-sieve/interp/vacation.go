package interp

import (
	"context"
	"encoding/gob"

	"github.com/foxcpp/go-sieve/parser"
)

// ActionVacation carries a vacation auto-reply request (RFC 5230), including
// the :handle parameter from RFC 8580. The host MTA is responsible for
// recipient safety checks, persistent deduplication and reply construction.
type ActionVacation struct {
	Reason    string
	Days      int
	Subject   string
	From      string
	Addresses []string
	MIME      bool
	Handle    string
}

func (ActionVacation) testActionName() string { return "vacation" }

// cancelsImplicitKeep reports false: vacation never cancels the implicit keep,
// so the original message is still delivered while the reply is generated.
func (ActionVacation) cancelsImplicitKeep() bool { return false }

type CmdVacation struct {
	action ActionVacation
}

func (c CmdVacation) Execute(ctx context.Context, d *RuntimeData) error {
	c.action.Reason = expandVars(d, c.action.Reason)
	return d.OnAction(ctx, c.action, d)
}

func loadVacation(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("vacation") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'vacation'")
	}
	cmd := CmdVacation{action: ActionVacation{Days: 7}}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{
			"days": {
				NeedsValue: true,
				MatchNum:   func(val int) { cmd.action.Days = val },
			},
			"subject": {
				NeedsValue:   true,
				MinStrCount:  1,
				MaxStrCount:  1,
				MatchStr:     func(val []string) { cmd.action.Subject = val[0] },
			},
			"from": {
				NeedsValue:  true,
				MinStrCount: 1,
				MaxStrCount: 1,
				MatchStr:    func(val []string) { cmd.action.From = val[0] },
			},
			"addresses": {
				NeedsValue: true,
				MatchStr:   func(val []string) { cmd.action.Addresses = val },
			},
			"mime": {
				MatchBool: func() { cmd.action.MIME = true },
			},
			"handle": {
				NeedsValue:  true,
				MinStrCount: 1,
				MaxStrCount: 1,
				MatchStr:    func(val []string) { cmd.action.Handle = val[0] },
			},
		},
		Pos: []SpecPosArg{
			{
				MinStrCount: 1,
				MaxStrCount: 1,
				MatchStr:    func(val []string) { cmd.action.Reason = val[0] },
			},
		},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	if err != nil {
		return nil, err
	}
	if cmd.action.Days < 0 {
		return nil, parser.ErrorAt(pcmd.Position, "vacation :days must not be negative")
	}

	return cmd, nil
}

func init() {
	gob.Register(CmdVacation{})
	gob.Register(ActionVacation{})
	commands["vacation"] = loadVacation
	supportedRequires["vacation"] = struct{}{}
}
