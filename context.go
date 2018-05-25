package kong

import (
	"fmt"
	"reflect"
	"strings"
)

// ParseTrace records the nodes and parsed values from the current command-line.
type ParseTrace struct {
	// One of these will be non-nil.
	Positional *Value
	Flag       *Flag
	Argument   *Argument
	Command    *Command

	// Parsed value for non-commands.
	Value reflect.Value
}

type ParseContext struct {
	Trace []*ParseTrace // A trace through parsed nodes.
	Error error         // Error that occurred during trace, if any.

	command []string // Full command path.
	flags   []*Flag  // Accumulated available flags.
	node    *Node    // Current node being parsed.

	args []string
	app  *Application
	scan *Scanner
}

// Trace parses the command-line, validating and collecting matching grammar nodes.
func Trace(args []string, app *Application) (*ParseContext, error) {
	p := &ParseContext{
		app:  app,
		args: args,
	}
	err := p.reset(&p.app.Node)
	if err != nil {
		return nil, err
	}
	p.Error = p.trace(&p.app.Node)
	return p, nil
}

// FlagValue returns the set value of a flag, if it was encountered and exists.
func (p *ParseContext) FlagValue(flag *Flag) reflect.Value {
	for _, trace := range p.Trace {
		if trace.Flag == flag {
			return trace.Value
		}
	}
	return reflect.Value{}
}

// Recursively reset values to defaults (as specified in the grammar) or the zero value.
func (p *ParseContext) reset(node *Node) error {
	p.scan = Scan(p.args...)
	for _, flag := range node.Flags {
		err := flag.Value.Reset()
		if err != nil {
			return err
		}
	}
	for _, pos := range node.Positional {
		err := pos.Reset()
		if err != nil {
			return err
		}
	}
	for _, branch := range node.Children {
		if branch.Argument != nil {
			arg := branch.Argument.Argument
			err := arg.Reset()
			if err != nil {
				return err
			}
			p.reset(&branch.Argument.Node)
		} else {
			p.reset(branch.Command)
		}
	}
	return nil
}

func (p *ParseContext) trace(node *Node) (err error) { // nolint: gocyclo
	positional := 0
	p.node = node
	p.flags = append(p.flags, node.Flags...)

	for !p.scan.Peek().IsEOL() {
		token := p.scan.Peek()
		switch token.Type {
		case UntypedToken:
			switch {
			// -- indicates end of parsing. All remaining arguments are treated as positional arguments only.
			case token.Value == "--":
				p.scan.Pop()
				args := []string{}
				for {
					token = p.scan.Pop()
					if token.Type == EOLToken {
						break
					}
					args = append(args, token.Value)
				}
				// Note: tokens must be pushed in reverse order.
				for i := range args {
					p.scan.PushTyped(args[len(args)-1-i], PositionalArgumentToken)
				}

			// Long flag.
			case strings.HasPrefix(token.Value, "--"):
				p.scan.Pop()
				// Parse it and push the tokens.
				parts := strings.SplitN(token.Value[2:], "=", 2)
				if len(parts) > 1 {
					p.scan.PushTyped(parts[1], FlagValueToken)
				}
				p.scan.PushTyped(parts[0], FlagToken)

			// Short flag.
			case strings.HasPrefix(token.Value, "-"):
				p.scan.Pop()
				// Note: tokens must be pushed in reverse order.
				p.scan.PushTyped(token.Value[2:], ShortFlagTailToken)
				p.scan.PushTyped(token.Value[1:2], ShortFlagToken)

			default:
				p.scan.Pop()
				p.scan.PushTyped(token.Value, PositionalArgumentToken)
			}

		case ShortFlagTailToken:
			p.scan.Pop()
			// Note: tokens must be pushed in reverse order.
			p.scan.PushTyped(token.Value[1:], ShortFlagTailToken)
			p.scan.PushTyped(token.Value[0:1], ShortFlagToken)

		case FlagToken:
			if err := p.matchFlags(func(f *Flag) bool {
				return f.Name == token.Value
			}); err != nil {
				return err
			}

		case ShortFlagToken:
			if err := p.matchFlags(func(f *Flag) bool {
				return string(f.Name) == token.Value
			}); err != nil {
				return err
			}

		case FlagValueToken:
			return fmt.Errorf("unexpected flag argument %q", token.Value)

		case PositionalArgumentToken:
			// Ensure we've consumed all positional arguments.
			if positional < len(node.Positional) {
				arg := node.Positional[positional]
				value, err := arg.Parse(p.scan)
				if err != nil {
					return err
				}
				p.command = append(p.command, "<"+arg.Name+">")
				p.Trace = append(p.Trace, &ParseTrace{Positional: arg, Value: value})
				positional++
				break
			}

			// After positional arguments have been consumed, handle commands and branching arguments.
			for _, branch := range node.Children {
				switch {
				case branch.Command != nil:
					if branch.Command.Name == token.Value {
						p.scan.Pop()
						p.command = append(p.command, branch.Command.Name)
						p.Trace = append(p.Trace, &ParseTrace{Command: branch.Command})
						return p.trace(branch.Command)
					}

				case branch.Argument != nil:
					arg := branch.Argument.Argument
					if value, err := arg.Parse(p.scan); err == nil {
						p.command = append(p.command, "<"+arg.Name+">")
						p.Trace = append(p.Trace, &ParseTrace{Argument: branch.Argument, Value: value})
						return p.trace(&branch.Argument.Node)
					}
				}
			}
			return fmt.Errorf("unexpected positional argument %s", token)

		default:
			return fmt.Errorf("unexpected token %s", token)
		}
	}

	if err := checkMissingPositionals(positional, node.Positional); err != nil {
		return err
	}

	if err := checkMissingChildren(node.Children); err != nil {
		return err
	}

	if err := checkMissingFlags(node.Children, p.flags); err != nil {
		return err
	}

	return nil
}

// Apply traced context to the target grammar.
func (p *ParseContext) Apply() (string, error) {
	path := []string{}
	for _, trace := range p.Trace {
		switch {
		case trace.Argument != nil:
			path = append(path, "<"+trace.Argument.Name+">")
			trace.Argument.Argument.Apply(trace.Value)
		case trace.Command != nil:
			path = append(path, trace.Command.Name)
		case trace.Flag != nil:
			trace.Flag.Value.Apply(trace.Value)
		case trace.Positional != nil:
			path = append(path, "<"+trace.Positional.Name+">")
			trace.Positional.Apply(trace.Value)
		}
	}
	return strings.Join(path, " "), nil
}

func checkMissingFlags(children []*Branch, flags []*Flag) error {
	// Only check required missing fields at the last child.
	if len(children) > 0 {
		return nil
	}
	missing := []string{}
	for _, flag := range flags {
		if !flag.Required || flag.Set {
			continue
		}
		missing = append(missing, flag.Name)
	}
	if len(missing) == 0 {
		return nil
	}

	return fmt.Errorf("missing flags: %s", strings.Join(missing, ", "))
}

func checkMissingChildren(children []*Branch) error {
	missing := []string{}
	for _, child := range children {
		if child.Argument != nil {
			if !child.Argument.Argument.Required {
				continue
			}
			missing = append(missing, "<"+child.Argument.Name+">")
		} else {
			missing = append(missing, child.Command.Name)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	return fmt.Errorf("expected one of %s", strings.Join(missing, ", "))
}

// If we're missing any positionals and they're required, return an error.
func checkMissingPositionals(positional int, values []*Value) error {
	// All the positionals are in.
	if positional == len(values) {
		return nil
	}

	// We're low on supplied positionals, but the missing one is optional.
	if !values[positional].Required {
		return nil
	}

	missing := []string{}
	for ; positional < len(values); positional++ {
		missing = append(missing, "<"+values[positional].Name+">")
	}
	return fmt.Errorf("missing positional arguments %s", strings.Join(missing, " "))
}

func (p *ParseContext) matchFlags(matcher func(f *Flag) bool) (err error) {
	token := p.scan.Peek()
	defer func() {
		msg := recover()
		if test, ok := msg.(Error); ok {
			err = fmt.Errorf("%s %s", token, test)
		} else if msg != nil {
			panic(msg)
		}
	}()
	for _, flag := range p.flags {
		// Found a matching flag.
		if flag.Name == token.Value {
			p.scan.Pop()
			value, err := flag.Parse(p.scan)
			if err != nil {
				return err
			}
			p.Trace = append(p.Trace, &ParseTrace{Flag: flag, Value: value})
			return nil
		}
	}
	return fmt.Errorf("unknown flag --%s", token.Value)
}
