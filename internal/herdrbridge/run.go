package herdrbridge

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/zjpiazza/sandherd/internal/buildinfo"
)

func Run(arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandherd-herdr-bridge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version information and exit")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: sandherd-herdr-bridge [--version] <action|manager|attach>")
	}
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *showVersion {
		buildinfo.Write(stdout, "sandherd-herdr-bridge")
		return 0
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		flags.Usage()
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	herdr := NewHerdrClient(nil)
	switch remaining[0] {
	case "action":
		if len(remaining) != 2 {
			fmt.Fprintln(stderr, "action requires one action name")
			return 2
		}
		if err := herdr.OpenManager(ctx, remaining[1], actionTargetPane()); err != nil {
			fmt.Fprintf(stderr, "sandherd-herdr-bridge: %v\n", err)
			return 1
		}
		return 0
	case "manager":
		if len(remaining) != 1 {
			fmt.Fprintln(stderr, "manager does not accept arguments")
			return 2
		}
		configuration, api, store, err := loadRuntime()
		if err != nil {
			fmt.Fprintf(stderr, "sandherd-herdr-bridge: %v\n", err)
			return 1
		}
		manager := NewManager(configuration, api, store, herdr, stdin, stdout)
		if err := manager.Run(ctx, os.Getenv("SANDHERD_ACTION")); err != nil {
			_ = herdr.Notify(ctx, "Sandherd action failed", err.Error(), "request")
			fmt.Fprintf(stderr, "sandherd-herdr-bridge: %v\n", err)
			return 1
		}
		return 0
	case "attach":
		attachFlags := flag.NewFlagSet("attach", flag.ContinueOnError)
		attachFlags.SetOutput(stderr)
		agentID := attachFlags.String("agent-id", os.Getenv("SANDHERD_AGENT_ID"), "Sandherd agent UUID")
		takeover := attachFlags.Bool("takeover", os.Getenv("SANDHERD_TAKEOVER") == "1", "explicitly take over the controller lease")
		if err := attachFlags.Parse(remaining[1:]); err != nil || attachFlags.NArg() != 0 {
			return 2
		}
		inputFile, ok := stdin.(*os.File)
		if !ok {
			fmt.Fprintln(stderr, "sandherd-herdr-bridge: attach requires a terminal file as stdin")
			return 1
		}
		configuration, api, store, err := loadRuntime()
		if err != nil {
			fmt.Fprintf(stderr, "sandherd-herdr-bridge: %v\n", err)
			return 1
		}
		bridge, err := NewBridge(BridgeOptions{
			Config: configuration, API: api, Store: store, Herdr: herdr,
			AgentID: strings.TrimSpace(*agentID), PaneID: CurrentPaneID(), Takeover: *takeover,
			Input: inputFile, Output: stdout, Status: stderr,
		})
		if err == nil {
			err = bridge.Run(ctx)
		}
		if err != nil {
			fmt.Fprintf(stderr, "\r\nsandherd-herdr-bridge: %v\r\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", remaining[0])
		flags.Usage()
		return 2
	}
}

func loadRuntime() (Config, *Client, *StateStore, error) {
	configuration, err := LoadConfig()
	if err != nil {
		return Config{}, nil, nil, err
	}
	api, err := NewClient(configuration, nil)
	if err != nil {
		return Config{}, nil, nil, err
	}
	stateDirectory, err := StateDirectory()
	if err != nil {
		return Config{}, nil, nil, err
	}
	store, err := NewStateStore(stateDirectory)
	if err != nil {
		return Config{}, nil, nil, err
	}
	return configuration, api, store, nil
}
