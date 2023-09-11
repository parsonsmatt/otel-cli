package otelcli

import (
    // "errors"
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
        "os/signal"
        "syscall"
	"os/exec"
	"strings"
	"time"

	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"github.com/equinix-labs/otel-cli/otlpclient"
	"github.com/spf13/cobra"
)

// execCmd sets up the `otel-cli exec` command
func execCmd(config *Config) *cobra.Command {
	cmd := cobra.Command{
		Use:   "exec",
		Short: "execute the command provided",
		Long: `execute the command provided after the subcommand inside a span, measuring
and reporting how long it took to run. The wrapping span's w3c traceparent is automatically
passed to the child process's environment as TRACEPARENT.

Examples:

otel-cli exec -n my-cool-thing -s interesting-step curl https://cool-service/api/v1/endpoint

otel-cli exec -s "outer span" 'otel-cli exec -s "inner span" sleep 1'

WARNING: this does not clean or validate the command at all before passing it
to sh -c and should not be passed any untrusted input`,
		Run:  doExec,
		Args: cobra.MinimumNArgs(1),
	}

	addCommonParams(&cmd, config)
	addSpanParams(&cmd, config)
	addAttrParams(&cmd, config)
	addClientParams(&cmd, config)

	return &cmd
}

func doExec(cmd *cobra.Command, args []string) {
	ctx := cmd.Context()
	config := getConfig(ctx)
	ctx, client := StartClient(ctx, config)

	// put the command in the attributes, before creating the span so it gets picked up
	config.Attributes["command"] = args[0]
	config.Attributes["arguments"] = ""

	var child *exec.Cmd
	if len(args) > 1 {
		// CSV-join the arguments to send as an attribute
		buf := bytes.NewBuffer([]byte{})
		csv.NewWriter(buf).WriteAll([][]string{args[1:]})
		config.Attributes["arguments"] = buf.String()

		child = exec.Command(args[0], args[1:]...)
	} else {
		child = exec.Command(args[0])
	}

	// attach all stdio to the parent's handles
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	// pass the existing env but add the latest TRACEPARENT carrier so e.g.
	// otel-cli exec 'otel-cli exec sleep 1' will relate the spans automatically
	child.Env = []string{}

	// grab everything BUT the TRACEPARENT envvar
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "TRACEPARENT=") {
			child.Env = append(child.Env, env)
		}
	}

	span := config.NewProtobufSpan()

	// set the traceparent to the current span to be available to the child process
	if config.GetIsRecording() {
		tp := otlpclient.TraceparentFromProtobufSpan(span, config.GetIsRecording())
		child.Env = append(child.Env, fmt.Sprintf("TRACEPARENT=%s", tp.Encode()))
		// when not recording, and a traceparent is available, pass it through
	} else if !config.TraceparentIgnoreEnv {
		tp := config.LoadTraceparent()
		if tp.Initialized {
			child.Env = append(child.Env, fmt.Sprintf("TRACEPARENT=%s", tp.Encode()))
		}
	}

        c := make(chan os.Signal, 100)
        signal.Notify(c, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
        // ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
        // defer stop()
        doneChan := make(chan bool)
        finishSpan := func () {
            fmt.Println("finishing span")
            span.EndTimeUnixNano = uint64(time.Now().UnixNano())

            fmt.Println("sending span...")
            fmt.Println("config.GetIsRecording(): ", config.GetIsRecording())
            ctx, err := otlpclient.SendSpan(ctx, client, config, span)
            if err != nil {
                    fmt.Println("unable to send span", err)
                    config.SoftFail("unable to send span: %s", err)
            }
            fmt.Println("span spent??")

            _, err = client.Stop(ctx)
            if err != nil {
                    config.SoftFail("client.Stop() failed: %s", err)
            }
            fmt.Println("client stopped")

            // set the global exit code so main() can grab it and os.Exit() properly
            Diag.ExecExitCode = child.ProcessState.ExitCode()
            fmt.Println("exit code set")

            config.PropagateTraceparent(span, os.Stdout)
            fmt.Println("propagate traceparent done")
        }
        go func() {
            for sig := range c {
                fmt.Println("sig: ", sig)
                finishSpan()
                doneChan <- true
            }

        }()

	if err := child.Run(); err != nil {
                span.Status = & tracev1.Status {
                        Message: fmt.Sprintln("exec command failed: ", err),
                        Code: tracev1.Status_STATUS_CODE_ERROR,
                }
	}
        defer finishSpan()
        wait := func () {
            fmt.Println("in wait")
            select {
            case <- doneChan:
                fmt.Println("done pulled")
            }
        }
        fmt.Println("about to call wait")
        wait()
}
