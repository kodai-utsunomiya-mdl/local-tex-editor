package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/app"
	"github.com/kodai-utsunomiya-mdl/local-tex-editor/internal/platform"
	"github.com/kodai-utsunomiya-mdl/local-tex-editor/web"
)

var version = "0.1.3"
var noBrowser bool

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if !noBrowser {
			platform.ShowError(err.Error())
		}
		os.Exit(1)
	}
}

func run() error {
	project := flag.String("project", "", "TeX project folder")
	texBin := flag.String("tex-bin", "", "TeX Live executables folder")
	listen := flag.String("listen", "127.0.0.1:0", "loopback address to listen on")
	flag.BoolVar(&noBrowser, "no-browser", false, "do not open a browser automatically")
	readyFile := flag.String("ready-file", "", "write server information to this file")
	configDir := flag.String("config-dir", "", "application settings and cache folder")
	showVersion := flag.Bool("version", false, "show application version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *project == "" && flag.NArg() == 1 {
		*project = flag.Arg(0)
	}
	address, err := net.ResolveTCPAddr("tcp", *listen)
	if err != nil || address.IP == nil || !address.IP.IsLoopback() {
		return errors.New("--listen must use a loopback IP address, for example 127.0.0.1:0")
	}
	if *configDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		*configDir = filepath.Join(base, "LocalTeXEditor")
	}
	*configDir, err = filepath.Abs(*configDir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(*configDir, 0700); err != nil {
		return err
	}
	runContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()
	releaseInstance, update, err := acquireStartupInstance(runContext, *configDir, *readyFile, noBrowser, platform.OpenBrowser)
	if err != nil {
		return fmt.Errorf("アプリを起動できません: %w", err)
	}
	if releaseInstance == nil {
		return nil
	}
	defer releaseInstance()
	if update != nil {
		defer update.Close()
	}
	installationError := ""
	if !noBrowser {
		if err := platform.RefreshInstalledShortcuts(); err != nil {
			installationError = err.Error()
		}
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return fmt.Errorf("could not start the local server: %w", err)
	}
	defer listener.Close()
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return err
	}
	token := hex.EncodeToString(secret)
	quit := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(quit) }) }
	application, err := app.New(app.Options{ConfigDir: *configDir, ProjectPath: *project, TexBin: *texBin, Token: token, Version: version, Shutdown: stop, InstallationError: installationError})
	if err != nil {
		return err
	}
	defer application.Close()
	host := listener.Addr().String()
	application.SetAddress(host)
	url := "http://" + host + "/"
	mux := http.NewServeMux()
	mux.Handle("/api/", application.Handler())
	mux.Handle("/", staticFiles(host, web.Files()))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	ready := map[string]any{"url": url, "token": token, "pid": os.Getpid(), "version": version}
	data, _ := json.Marshal(ready)
	instanceFile := filepath.Join(*configDir, "active.json")
	if err = os.WriteFile(instanceFile, append(data, '\n'), 0600); err != nil {
		return err
	}
	defer os.Remove(instanceFile)
	if *readyFile != "" {
		if err = os.MkdirAll(filepath.Dir(*readyFile), 0700); err != nil {
			return err
		}
		if err = os.WriteFile(*readyFile, append(data, '\n'), 0600); err != nil {
			return err
		}
		defer os.Remove(*readyFile)
	}
	fmt.Println(string(data))
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	if update != nil || !noBrowser {
		application.ExpectBrowserConnection(30 * time.Second)
	}
	if update != nil {
		if !update.Complete(url + "#token=" + token) {
			return nil
		}
	} else if !noBrowser {
		if err = platform.OpenBrowser(url + "#token=" + token); err != nil {
			return err
		}
	}
	select {
	case <-quit:
	case <-runContext.Done():
	case err = <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	application.CloseBrowserSessions()
	return server.Shutdown(ctx)
}
