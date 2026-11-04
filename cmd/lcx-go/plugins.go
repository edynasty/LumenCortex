package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"

	lcx "github.com/edynasty/LumenCortex/runtime"
)

// splitFlagArgs separates flags (with their values, when the flag takes one) from positional
// arguments, so `plugins apply exa --scope global` and `plugins apply --scope global exa` both work:
// the flag package alone stops at the first positional argument.
func splitFlagArgs(fs *flag.FlagSet, args []string) (flags, positional []string) {
	takesValue := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if boolean, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			return
		}
		takesValue[f.Name] = true
	})
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if takesValue[name] && index+1 < len(args) {
			flags = append(flags, args[index+1])
			index++
		}
	}
	return flags, positional
}

const pluginsUsage = `usage:
  lcx-go plugins [list]                       registered in-process plugins
  lcx-go plugins enable <id> | disable <id>   enable or disable one plugin
  lcx-go plugins capabilities [--denylist a,b] [--capability c] [--session id]
                                              every tool a run could be offered, with source,
                                              permission, availability and requirements
  lcx-go plugins cost [--session id]          tool-schema bytes a request carries, by source
  lcx-go plugins templates                    shipped MCP server templates
  lcx-go plugins apply <template> [--scope global|project]
                                              write a template as a disabled mcp.json entry
  lcx-go plugins server <enable|disable> <id> [--scope global|project]
                                              enable or disable a configured MCP server`

// runPluginsCommand shows the plugin catalog, the capability registry and the shipped MCP templates,
// and changes enablement through the state that already owns it (ADR-0012, ADR-0021).
func runPluginsCommand(ctx context.Context, engine *lcx.Engine, args []string) error {
	if len(args) == 0 || (args[0] == "list" && len(args) == 1) {
		return printJSON(engine.Plugins())
	}
	switch args[0] {
	case "enable", "disable":
		if len(args) != 2 {
			return errors.New(pluginsUsage)
		}
		if err := engine.SetPluginEnabled(ctx, args[1], args[0] == "enable"); err != nil {
			return err
		}
		return printJSON(engine.Plugins())
	case "capabilities":
		fs := flag.NewFlagSet("plugins capabilities", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		denylist := fs.String("denylist", "", "comma-separated tool ids or providers to report as disabled")
		capability := fs.String("capability", "", "only entries of this capability")
		session := fs.String("session", "", "session whose workspace to inspect")
		flags, positional := splitFlagArgs(fs, args[1:])
		if err := fs.Parse(flags); err != nil || len(positional) != 0 {
			return errors.New(pluginsUsage)
		}
		query := lcx.CapabilityQuery{SessionID: *session, Capability: *capability}
		for _, name := range strings.Split(*denylist, ",") {
			if name = strings.TrimSpace(name); name != "" {
				query.Denylist = append(query.Denylist, name)
			}
		}
		capabilities, err := engine.Capabilities(ctx, query)
		if err != nil {
			return err
		}
		return printJSON(capabilities)
	case "cost":
		fs := flag.NewFlagSet("plugins cost", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		session := fs.String("session", "", "session whose workspace to inspect")
		flags, positional := splitFlagArgs(fs, args[1:])
		if err := fs.Parse(flags); err != nil || len(positional) != 0 {
			return errors.New(pluginsUsage)
		}
		cost, err := engine.ToolSchemaCost(ctx, lcx.CapabilityQuery{SessionID: *session})
		if err != nil {
			return err
		}
		return printJSON(cost)
	case "templates":
		if len(args) != 1 {
			return errors.New(pluginsUsage)
		}
		return printJSON(lcx.MCPTemplates())
	case "apply":
		fs := flag.NewFlagSet("plugins apply", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		scope := fs.String("scope", "project", "configuration scope: global or project")
		flags, positional := splitFlagArgs(fs, args[1:])
		if err := fs.Parse(flags); err != nil || len(positional) != 1 {
			return errors.New(pluginsUsage)
		}
		cfg, err := engine.ApplyMCPTemplate(ctx, *scope, positional[0])
		if err != nil {
			return err
		}
		return printJSON(cfg)
	case "server":
		fs := flag.NewFlagSet("plugins server", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		scope := fs.String("scope", "project", "configuration scope: global or project")
		flags, positional := splitFlagArgs(fs, args[1:])
		if err := fs.Parse(flags); err != nil || len(positional) != 2 {
			return errors.New(pluginsUsage)
		}
		switch positional[0] {
		case "enable":
			if err := engine.SetMCPServerEnabled(ctx, *scope, positional[1], true); err != nil {
				return err
			}
		case "disable":
			if err := engine.SetMCPServerEnabled(ctx, *scope, positional[1], false); err != nil {
				return err
			}
		default:
			return errors.New(pluginsUsage)
		}
		configs, err := engine.MCPConfigsScope(*scope)
		if err != nil {
			return err
		}
		return printJSON(configs)
	default:
		return errors.New(pluginsUsage)
	}
}
