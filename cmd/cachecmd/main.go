package main

import (
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const version = "0.9.0"

const usageMessage = "" +
	`Usage: cachecmd [flags] command
`

func usage() {
	fmt.Fprintln(os.Stderr, usageMessage)
	fmt.Fprintln(os.Stderr, "Flags:")
	flag.PrintDefaults()
	os.Exit(2)
}

type option struct {
	version bool
	ttl     time.Duration
	async   bool
}

var flagOpt = &option{}

func init() {
	flag.BoolVar(&flagOpt.version, "version", false, "print version")
	flag.DurationVar(&flagOpt.ttl, "ttl", time.Minute, "TTL(Time to live) of cache")
	flag.BoolVar(&flagOpt.async, "async", false,
		"return result from cache immidiately and update cache in background")
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if flagOpt.version {
		fmt.Fprintln(os.Stderr, version)
		return
	}
	if err := run(os.Stdin, os.Stdout, os.Stderr, flagOpt, flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "cachecmd: %v\n", err)
		os.Exit(exitCode(err))
	}
}

func run(r io.Reader, stdout, stderr io.WriteCloser, opt *option, command []string) error {
	if len(command) == 0 {
		usage()
		return nil
	}
	cachecmd := CacheCmd{
		stdout:  stdout,
		stderr:  stderr,
		cmdName: command[0],
		cmdArgs: command[1:],
		opt:     opt,
	}
	return cachecmd.Run(context.Background())
}

type CacheCmd struct {
	stdout  io.WriteCloser
	stderr  io.WriteCloser
	cmdName string
	cmdArgs []string
	opt     *option

	currentTime  time.Time
	cachecmdExec string
}

func (c *CacheCmd) Run(ctx context.Context) error {
	if err := c.makeCacheDir(); err != nil {
		return err
	}

	cachePath := c.cacheFilePath()

	// Read from cache.
	if c.shouldUseCache(cachePath) {
		if err := c.fromCache(cachePath); err != nil {
			return err
		}
		if !c.opt.async {
			return nil
		}
		// Spawn update command in background and return.
		return c.updateCacheCmd().Start()
	}

	// Create temp file to store command result.
	// Do not use cache file directly to access cache file while updating cache.
	tmpf, err := ioutil.TempFile("", "cachecmd_")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpf.Name())

	// Run command.
	if err := c.runCmd(ctx, tmpf); err != nil {
		return err
	}

	// Rename temp file to appropriate file name for cache.
	err = tmpf.Close()
	if err = os.Rename(tmpf.Name(), cachePath); err != nil {
		return fmt.Errorf("failed to update cache: %v", err)
	}

	return nil
}

func (c *CacheCmd) updateCacheCmd() *exec.Cmd {
	execName := c.cachecmdExec
	if execName == "" {
		execName = os.Args[0]
	}
	args := append(c.cmdArgs[:0],
		append([]string{"-ttl=0", c.cmdName}, c.cmdArgs[0:]...)...)
	return exec.Command(execName, args...)
}

func (c *CacheCmd) shouldUseCache(cacheFname string) bool {
	if !fileexists(cacheFname) {
		return false
	}
	stat, err := os.Stat(cacheFname)
	if err != nil {
		return false
	}
	if c.currentTime.Second() == 0 {
		c.currentTime = time.Now()
	}
	return c.currentTime.Add(-c.opt.ttl).Sub(stat.ModTime()).Seconds() < 0
}

func (c *CacheCmd) fromCache(cacheFname string) error {
	f, err := os.Open(cacheFname)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(c.stdout, f); err != nil {
		return err
	}
	return nil
}

func (c *CacheCmd) makeCacheDir() error {
	return os.MkdirAll(cacheDir(), os.ModePerm)
}

func (c *CacheCmd) cacheFilePath() string {
	fname := cacheFileName(c.cmdName + " " + strings.Join(c.cmdArgs, " "))
	return filepath.Join(cacheDir(), fname)
}

func (c *CacheCmd) runCmd(ctx context.Context, cache io.Writer) error {
	cmd := exec.CommandContext(ctx, c.cmdName, c.cmdArgs...)
	cmd.Stderr = c.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	if _, err := io.Copy(cache, io.TeeReader(stdout, c.stdout)); err != nil {
		return fmt.Errorf("failed to copy command result to cache: %v", err)
	}

	return cmd.Wait()
}

func fileexists(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

func cacheFileName(cmd string) string {
	h := md5.New()
	io.WriteString(h, cmd)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func cacheDir() string {
	return filepath.Join(xdgCacheHome(), "cachecmd")
}

// REF: https://specifications.freedesktop.org/basedir-spec/basedir-spec-0.6.html
func xdgCacheHome() string {
	path := os.Getenv("XDG_CACHE_HOME")
	if path == "" {
		path = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return path
}

func exitCode(err error) int {
	if exiterr, ok := err.(*exec.ExitError); ok {
		if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
	}
	return 1
}