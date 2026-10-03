// On-Call Compensation Calculator: a single executable with a local web UI and a CLI.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	_ "time/tzdata" // time zone database embedded, so the binary has no dependencies

	"oncallcalc/internal/export"
	"oncallcalc/internal/pd"
	"oncallcalc/internal/report"
	"oncallcalc/internal/server"
	"oncallcalc/internal/store"
)

//go:embed web
var webFS embed.FS

const usage = `On-Call Compensation Calculator

Usage:
  oncallcalc [--port N] [--no-browser]      start the local web UI (default)
  oncallcalc run --report NAME --from YYYY-MM-DD --to YYYY-MM-DD [--format csv,pdf] [--out DIR]
                                            run a saved report headless
  oncallcalc list                           list saved reports and connections
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "run":
			os.Exit(cliRun(args[1:]))
		case "list":
			os.Exit(cliList())
		case "help", "-h", "--help":
			fmt.Print(usage)
			return
		}
	}
	fs0 := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs0.Int("port", 8787, "port to listen on (localhost only)")
	noBrowser := fs0.Bool("no-browser", false, "do not open the browser")
	fs0.Parse(args)

	st, err := store.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open data folder:", err)
		os.Exit(1)
	}
	web, _ := fs.Sub(webFS, "web")
	srv := server.New(st, web)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0") // port busy: pick a free one
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", ln.Addr().(*net.TCPAddr).Port, srv.Token)
	fmt.Println("On-Call Compensation Calculator is running. Open this link if your browser does not open:")
	fmt.Println("  " + url)
	fmt.Println("Press Ctrl+C to stop.")
	if !*noBrowser {
		openBrowser(url)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		hs.Shutdown(context.Background())
	}()
	if err := hs.Serve(ln); err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func cliList() int {
	st, err := store.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cs, _ := st.Connections()
	rs, _ := st.Reports()
	fmt.Println("Connections:")
	for _, c := range cs {
		fmt.Printf("  %s  (%s, key ending %s)\n", c.Name, c.Region, c.KeyTail)
	}
	fmt.Println("Saved reports:")
	for _, r := range rs {
		fmt.Printf("  %s\n", r.Name)
	}
	return 0
}

func cliRun(args []string) int {
	f := flag.NewFlagSet("run", flag.ExitOnError)
	name := f.String("report", "", "saved report name")
	from := f.String("from", "", "first day, YYYY-MM-DD")
	to := f.String("to", "", "last day, YYYY-MM-DD")
	formats := f.String("format", "csv", "comma-separated: csv,csv_detail,json,xlsx,html,pdf")
	out := f.String("out", ".", "output folder")
	delay := f.Int("delay-ms", 500, "delay between API calls")
	retries := f.Int("retries", 6, "max retries per call")
	f.Parse(args)
	if *name == "" || *from == "" || *to == "" {
		fmt.Print(usage)
		return 2
	}
	st, err := store.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rs, _ := st.Reports()
	var saved *store.SavedReport
	for i := range rs {
		if strings.EqualFold(rs[i].Name, *name) {
			saved = &rs[i]
		}
	}
	if saved == nil {
		fmt.Fprintf(os.Stderr, "no saved report named %q (run `oncallcalc list`)\n", *name)
		return 1
	}
	var cfg report.Config
	if err := json.Unmarshal(saved.Config, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "saved report is unreadable:", err)
		return 1
	}
	cfg.From, cfg.To = *from, *to
	tok, err := st.Token(saved.ConnID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "API key not found in the credential store; add the connection in the web UI first")
		return 1
	}
	region := "us"
	cs, _ := st.Connections()
	for _, c := range cs {
		if c.ID == saved.ConnID {
			region = c.Region
		}
	}
	cl := pd.New(tok, region, pd.Settings{DelayMS: *delay, MaxRetries: *retries, TimeoutS: 30})
	cl.OnEvent = func(e pd.Event) {
		if e.Kind == "retry" {
			fmt.Fprintln(os.Stderr, e.Msg)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := report.Run(ctx, cl, report.NewHolidays(st.Dir), cfg, func(stage string, d, t int) {
		fmt.Fprintf(os.Stderr, "\r%s %d/%d   ", stage, d, t)
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	_ = os.MkdirAll(*out, 0o755)
	for _, fm := range strings.Split(*formats, ",") {
		data, fn, _, err := export.Render(res, strings.TrimSpace(fm), export.Options{Title: cfg.Name, Detail: true, Rules: true})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		p := filepath.Join(*out, fn)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("wrote", p)
	}
	for _, p := range res.People {
		if len(p.Incomplete) > 0 {
			fmt.Fprintf(os.Stderr, "WARNING: data for %s is incomplete (see report)\n", p.Name)
		}
	}
	return 0
}
