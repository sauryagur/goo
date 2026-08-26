// Command goo is the CLI for Gur's Obvious Objects.
//
// It is a thin front-end over the engine package. Object operations
// (put/get/rm/ls/stat/events) talk to a local engine directly. `server`
// starts the HTTP + SSE API; `tui` launches the terminal UI.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gur/goo/internal/engine"
	"github.com/gur/goo/internal/goo"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	root := envOr("GOO_ROOT", defaultRoot())

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "server":
		err = cmdServer(args, root)
	case "tui":
		err = cmdTUI(args, root)
	case "put":
		err = cmdPut(args, root)
	case "get":
		err = cmdGet(args, root)
	case "rm":
		err = cmdRm(args, root)
	case "ls":
		err = cmdLs(args, root)
	case "stat":
		err = cmdStat(args, root)
	case "events":
		err = cmdEvents(args, root)
	case "version", "--version", "-v":
		fmt.Println("goo (Gur's Obvious Objects) dev")
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "goo: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "goo: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `goo — Gur's Obvious Objects

USAGE
  goo <command> [arguments]

COMMANDS
  server            start the HTTP + SSE server (default :8080)
  tui               launch the terminal UI
  put <bucket/key> <file>   store a file as an object
  get <bucket/key> [file]   fetch an object (stdout if no file)
  rm  <bucket/key>          delete an object
  ls  <bucket>              list objects in a bucket
  stat <bucket/key>         show object metadata
  events [bucket] [--from N]   print the durable event log

ENV
  GOO_ROOT   directory for object + log data (default: ./goo-data)
`)
}

func defaultRoot() string {
	return filepath.Join(".", "goo-data")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func open(root string) (*engine.Engine, error) {
	return engine.Open(root)
}

// splitRef splits "bucket/key" into its two parts, validating the input.
func splitRef(ref string) (bucket, key string, err error) {
	i := strings.Index(ref, "/")
	if i < 0 {
		return "", "", fmt.Errorf("expected bucket/key, got %q", ref)
	}
	bucket, key = ref[:i], ref[i+1:]
	if err := goo.CheckRef(bucket, key); err != nil {
		return "", "", err
	}
	return bucket, key, nil
}

func cmdPut(args []string, root string) error {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	overwrite := fs.Bool("f", true, "overwrite if the object already exists")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: goo put <bucket/key> <file>")
	}
	bucket, key, err := splitRef(fs.Arg(0))
	if err != nil {
		return err
	}
	data, err := os.Open(fs.Arg(1))
	if err != nil {
		return fmt.Errorf("open input file: %w", err)
	}
	defer data.Close()

	e, err := open(root)
	if err != nil {
		return err
	}
	defer e.Close()

	ev, err := e.Put(bucket, key, data, *overwrite)
	if err != nil {
		return err
	}
	fmt.Printf("PUT %s/%s -> seq %d version %d (%d bytes, %s)\n",
		ev.Bucket, ev.Key, ev.Sequence, ev.Version, ev.Size, shortHash(ev.Hash))
	return nil
}

func cmdGet(args []string, root string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fmt.Errorf("usage: goo get <bucket/key> [file]")
	}
	bucket, key, err := splitRef(fs.Arg(0))
	if err != nil {
		return err
	}
	e, err := open(root)
	if err != nil {
		return err
	}
	defer e.Close()

	rc, obj, err := e.Get(bucket, key)
	if err != nil {
		return err
	}
	defer rc.Close()

	var dst io.Writer = os.Stdout
	if fs.NArg() == 2 {
		f, err := os.Create(fs.Arg(1))
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}
		defer f.Close()
		dst = f
	}
	if _, err := io.Copy(dst, rc); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if fs.NArg() == 1 {
		return nil // wrote to stdout
	}
	fmt.Printf("GET %s/%s -> %d bytes (version %d, %s)\n",
		obj.Bucket, obj.Key, obj.Size, obj.Version, shortHash(obj.Hash))
	return nil
}

func cmdRm(args []string, root string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: goo rm <bucket/key>")
	}
	bucket, key, err := splitRef(fs.Arg(0))
	if err != nil {
		return err
	}
	e, err := open(root)
	if err != nil {
		return err
	}
	defer e.Close()

	ev, err := e.Delete(bucket, key)
	if err != nil {
		return err
	}
	fmt.Printf("DELETE %s/%s -> seq %d\n", ev.Bucket, ev.Key, ev.Sequence)
	return nil
}

func cmdLs(args []string, root string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: goo ls <bucket>")
	}
	bucket := fs.Arg(0)
	if !goo.ValidBucket(bucket) {
		return fmt.Errorf("%w: %q", goo.ErrInvalidBucket, bucket)
	}
	e, err := open(root)
	if err != nil {
		return err
	}
	defer e.Close()

	objs, err := e.List(bucket)
	if err != nil {
		return err
	}
	var total int64
	for _, o := range objs {
		total += o.Size
		fmt.Printf("%-40s %12d  v%-4d  %s\n", o.Key, o.Size, o.Version, shortHash(o.Hash))
	}
	fmt.Printf("\n%d objects, %d bytes\n", len(objs), total)
	return nil
}

func cmdStat(args []string, root string) error {
	fs := flag.NewFlagSet("stat", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: goo stat <bucket/key>")
	}
	bucket, key, err := splitRef(fs.Arg(0))
	if err != nil {
		return err
	}
	e, err := open(root)
	if err != nil {
		return err
	}
	defer e.Close()

	o, err := e.Stat(bucket, key)
	if err != nil {
		return err
	}
	fmt.Printf("bucket:    %s\n", o.Bucket)
	fmt.Printf("key:       %s\n", o.Key)
	fmt.Printf("version:   %d\n", o.Version)
	fmt.Printf("size:      %d bytes\n", o.Size)
	fmt.Printf("hash:      %s\n", o.Hash)
	fmt.Printf("created:   %s\n", o.CreatedAt.Format(time.RFC3339))
	fmt.Printf("updated:   %s\n", o.UpdatedAt.Format(time.RFC3339))
	return nil
}

func cmdEvents(args []string, root string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	from := fs.Uint64("from", 0, "start sequence (0 = beginning)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bucket := ""
	if fs.NArg() > 0 {
		bucket = fs.Arg(0)
		if !goo.ValidBucket(bucket) {
			return fmt.Errorf("%w: %q", goo.ErrInvalidBucket, bucket)
		}
	}
	e, err := open(root)
	if err != nil {
		return err
	}
	defer e.Close()

	evs, err := e.Log().Replay(*from)
	if err != nil {
		return err
	}
	for _, ev := range evs {
		if bucket != "" && ev.Bucket != bucket {
			continue
		}
		fmt.Printf("%6d %-7s %s/%s v%d %dB %s\n",
			ev.Sequence, ev.Action, ev.Bucket, ev.Key, ev.Version, ev.Size, ev.Timestamp.Format(time.RFC3339))
	}
	return nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
