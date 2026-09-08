// gh-ars 는 K8s 없이 정적 머신 위에서 GitHub Actions ephemeral runner 를 컨테이너로 오토스케일링하는
// 포그라운드 CLI 다. 서브커맨드는 둘뿐이다: run, scaleset delete. [§2, §11, DESIGN §9]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"gh-ars/internal/config"
	"gh-ars/internal/controller"
	"gh-ars/internal/github"
	"gh-ars/internal/logging"
)

const usage = `usage:
  gh-ars run -c <config.yaml> [--log-level debug|info|warn|error] [--log-format json|text]
  gh-ars scaleset delete <name> -c <config.yaml> [--log-level ...] [--log-format ...]
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// commonFlags 는 두 서브커맨드가 공유하는 플래그다. [§11]
type commonFlags struct {
	configPath string
	logLevel   string
	logFormat  string
}

func (f *commonFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.configPath, "c", "", "설정 파일 경로")
	fs.StringVar(&f.logLevel, "log-level", logging.DefaultLevel, "debug|info|warn|error")
	fs.StringVar(&f.logFormat, "log-format", logging.DefaultFormat, "json|text")
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "scaleset":
		if len(args) >= 2 && args[1] == "delete" {
			return cmdScaleSetDelete(args[2:], stdout, stderr)
		}
		fmt.Fprint(stderr, usage)
		return 2
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
}

// cmdRun: config → logging → github.New → Controller.Run. SIGINT/SIGTERM 으로 ctx 취소. [§7.1, §7.3, §11]
func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.bind(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cf.configPath == "" || fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	log, err := logging.New(stdout, cf.logLevel, cf.logFormat)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		log.Error("config invalid", "path", cf.configPath, "err", err)
		return 1
	}
	gh, err := github.New(cfg.GitHub, log)
	if err != nil {
		log.Error("github client", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctl := controller.New(cfg, gh, log, controller.Options{})
	if err := ctl.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("controller failed", "err", err)
		return 1
	}
	return 0
}

// cmdScaleSetDelete: 설정의 해당 scale set runnerGroup(없으면 Default)으로 조회 → 삭제. [§11]
func cmdScaleSetDelete(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scaleset delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.bind(fs)
	// `scaleset delete <name> -c <file>`: 이름이 플래그 앞에 온다.
	var name string
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if name == "" || cf.configPath == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	log, err := logging.New(stdout, cf.logLevel, cf.logFormat)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		log.Error("config invalid", "path", cf.configPath, "err", err)
		return 1
	}
	group := config.DefaultRunnerGroup
	if ss, ok := cfg.ScaleSet(name); ok {
		group = ss.RunnerGroup
	}
	gh, err := github.New(cfg.GitHub, log)
	if err != nil {
		log.Error("github client", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := gh.DeleteScaleSet(ctx, name, group); err != nil {
		log.Error("scale set delete failed", "scaleSet", name, "runnerGroup", group, "err", err)
		return 1
	}
	return 0
}
