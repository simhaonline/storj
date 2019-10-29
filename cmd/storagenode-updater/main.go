// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/storj/internal/errs2"
	"storj.io/storj/internal/fpath"
	"storj.io/storj/internal/sync2"
	"storj.io/storj/internal/version"
	"storj.io/storj/internal/version/checker"
	"storj.io/storj/pkg/cfgstruct"
	"storj.io/storj/pkg/identity"
	"storj.io/storj/pkg/process"
	"storj.io/storj/pkg/storj"
)

const (
	updaterServiceName = "storagenode-updater"
	minCheckInterval   = time.Minute
)

var (
	cancel context.CancelFunc
	// TODO: replace with config value of random bytes in storagenode config.
	nodeID storj.NodeID

	rootCmd = &cobra.Command{
		Use:   "storagenode-updater",
		Short: "Version updater for storage node",
	}
	runCmd = &cobra.Command{
		Use:   "run",
		Short: "Run the storagenode-updater for storage node",
		Args:  cobra.OnlyValidArgs,
		RunE:  cmdRun,
	}
	recoverCmd = &cobra.Command{
		Use:   "recover",
		Short: "Recover the storagenode-updater binary from a bad update using a backup",
		Args:  cobra.OnlyValidArgs,
		RunE:  cmdRecover,
	}

	runCfg struct {
		// TODO: check interval default has changed from 6 hours to 15 min.
		checker.Config
		Identity identity.Config

		BinaryLocation string `help:"the storage node executable binary location" default:"storagenode.exe"`
		ServiceName    string `help:"storage node OS service name" default:"storagenode"`
		Log string `help:"path to log file, if empty standard output will be used" default:""`
	}

	recoverCfg struct {
		Log string `help:"path to log file, if empty standard output will be used" default:""`
	}

	// NB: can't use `log.output` because windows service command args containing "." are bugged.
	//Log string `help:"path to log file, if empty standard output will be used" default:""`
	//logFlag = flag.String("log", "", "path to log file, if empty standard output will be used")

confDir     string
	identityDir string
)

type renameFunc func(currentVersion version.SemVer) error

func init() {
	// TODO: this will probably generate warnings for mismatched config fields.
	defaultConfDir := fpath.ApplicationDir("storj", "storagenode")
	defaultIdentityDir := fpath.ApplicationDir("storj", "identity", "storagenode")
	cfgstruct.SetupFlag(zap.L(), rootCmd, &confDir, "config-dir", defaultConfDir, "main directory for storagenode configuration")
	cfgstruct.SetupFlag(zap.L(), rootCmd, &identityDir, "identity-dir", defaultIdentityDir, "main directory for storagenode identity credentials")
	defaults := cfgstruct.DefaultsFlag(rootCmd)

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(recoverCmd)

	process.Bind(runCmd, &runCfg, defaults, cfgstruct.ConfDir(confDir), cfgstruct.IdentityDir(identityDir))
	process.Bind(recoverCmd, &recoverCfg, defaults, cfgstruct.ConfDir(confDir), cfgstruct.IdentityDir(identityDir))
}

func cmdRun(cmd *cobra.Command, args []string) (err error) {
	err, closeLog := openLog()
	defer func() { err = errs.Combine(err, closeLog()) }()

	if !fileExists(runCfg.BinaryLocation) {
		log.Fatal("unable to find storage node executable binary")
	}

	ident, err := runCfg.Identity.Load()
	if err != nil {
		log.Fatalf("error loading identity: %s", err)
	}
	nodeID = ident.ID
	if nodeID.IsZero() {
		log.Fatal("empty node ID")
	}

	var ctx context.Context
	ctx, cancel = process.Ctx(cmd)
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-c

		signal.Stop(c)
		cancel()
	}()

	loopFunc := func(ctx context.Context) (err error) {
		if err := update(ctx, runCfg.BinaryLocation, runCfg.ServiceName, renameStoragenode); err != nil {
			// don't finish loop in case of error just wait for another execution
			log.Println(err)
		}

		if err := update(ctx, os.Args[0], updaterServiceName, renameUpdater); err != nil {
			// TODO: remove
			log.Printf("updating storagenode-updater at \"%s\"", os.Args[0])
			// don't finish loop in case of error just wait for another execution
			// TODO: log.Println(err)
			log.Printf("%+v", err)
		}
		return nil
	}

	switch {
	case runCfg.CheckInterval <= 0:
		err = loopFunc(ctx)
	case runCfg.CheckInterval < minCheckInterval:
		log.Printf("check interval below minimum: \"%s\", setting to %s", runCfg.CheckInterval, minCheckInterval)
		runCfg.CheckInterval = minCheckInterval
		fallthrough
	default:
		loop := sync2.NewCycle(runCfg.CheckInterval)
		err = loop.Run(ctx, loopFunc)
	}
	if err != nil && errs2.IsCanceled(err) {
		log.Fatal(err)
	}
	return nil
}

func cmdRecover(cmd *cobra.Command, args []string) (err error) {
	err, closeLog := openLog()
	defer func() { err = errs.Combine(err, closeLog()) }()

	log.Println("storagenode updater recovering!")

	badExec := strings.Replace(os.Args[0], ".backup", "", 1)
	log.Printf("deleting bad updater binary: %s\n", badExec)
	err = os.Remove(badExec)
	if err != nil && !os.IsNotExist(err) {
		log.Println("")
	}

	log.Printf("restoring backup binary from: %s\n", os.Args[0])
	if err := os.Rename(os.Args[0], badExec); err != nil {
		return errs.Wrap(err)
	}
	return nil
}

func update(ctx context.Context, binPath, serviceName string, renameBinary renameFunc) (err error) {
	if nodeID.IsZero() {
		log.Fatal("empty node ID")
	}

	var currentVersion version.SemVer
	// TODO: remove
	log.Printf("checking version of %s", binPath)
	// TODO: remove
	log.Printf("serviceName: %s", serviceName)
	if serviceName == updaterServiceName {
		// TODO: remove
		log.Println("using version.Build.Version")
		currentVersion = version.Build.Version
	} else {
		// TODO: remove
		log.Println("using binaryVersion(binPath)")
		currentVersion, err = binaryVersion(binPath)
		if err != nil {
			return errs.Wrap(err)
		}
	}

	client := checker.New(runCfg.ClientConfig)
	log.Println("downloading versions from", runCfg.ServerAddress)
	shouldRollout, newVersion, err := client.ShouldRollout(ctx, serviceName, nodeID)
	if err != nil {
		return errs.Wrap(err)
	}

	if shouldRollout {
		// TODO: consolidate semver.Version and version.SemVer
		suggestedVersion, err := newVersion.SemVer()
		if err != nil {
			return errs.Wrap(err)
		}

		if currentVersion.Compare(suggestedVersion) < 0 {
			tempArchive, err := ioutil.TempFile(os.TempDir(), serviceName)
			if err != nil {
				return errs.New("cannot create temporary archive: %v", err)
			}
			defer func() { err = errs.Combine(err, os.Remove(tempArchive.Name())) }()

			downloadURL := parseDownloadURL(newVersion.URL)
			log.Println("start downloading", downloadURL, "to", tempArchive.Name())
			err = downloadArchive(ctx, tempArchive, downloadURL)
			if err != nil {
				return errs.Wrap(err)
			}
			log.Println("finished downloading", downloadURL, "to", tempArchive.Name())

			err = renameBinary(currentVersion)
			if err != nil {
				return errs.Wrap(err)
			}

			err = unpackBinary(ctx, tempArchive.Name(), binPath)
			if err != nil {
				return errs.Wrap(err)
			}

			downloadedVersion, err := binaryVersion(binPath)
			if err != nil {
				return errs.Wrap(err)
			}

			if suggestedVersion.Compare(downloadedVersion) != 0 {
				return errs.New("invalid version downloaded: wants %s got %s", suggestedVersion.String(), downloadedVersion.String())
			}

			log.Println("restarting service", serviceName)
			err = restartService(serviceName)
			if err != nil {
				// TODO: should we try to recover from this?
				return errs.New("unable to restart service: %v", err)
			}
			log.Println("service", serviceName, "restarted successfully")

			// TODO remove old binary ??
		} else {
			log.Printf("%s version is up to date\n", serviceName)
		}
	}
	return nil
}

func renameStoragenode(currentVersion version.SemVer) error {
	extension := filepath.Ext(runCfg.BinaryLocation)
	dir := filepath.Dir(runCfg.BinaryLocation)
	backupExec := filepath.Join(dir, runCfg.ServiceName+".old."+currentVersion.String()+extension)

	if err := os.Rename(runCfg.BinaryLocation, backupExec); err != nil {
		return errs.Wrap(err)
	}
	return nil
}

func renameUpdater(_ version.SemVer) error {
	updaterBinPath := os.Args[0]
	extension := filepath.Ext(updaterBinPath)
	dir := filepath.Dir(updaterBinPath)
	base := filepath.Base(updaterBinPath)
	base = base[:len(base)-len(extension)]
	backupExec := filepath.Join(dir, base+".backup"+extension)

	if err := os.Rename(updaterBinPath, backupExec); err != nil {
		return errs.Wrap(err)
	}
	return nil
}

func parseDownloadURL(template string) string {
	url := strings.Replace(template, "{os}", runtime.GOOS, 1)
	url = strings.Replace(url, "{arch}", runtime.GOARCH, 1)
	return url
}

func binaryVersion(location string) (version.SemVer, error) {
	// TODO: remove
	log.Printf("executing command %s", location+" version")
	out, err := exec.Command(location, "version").CombinedOutput()
	if err != nil {
		log.Printf("out %s", string(out))
		return version.SemVer{}, err
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		prefix := "Version: "
		if strings.HasPrefix(line, prefix) {
			line = line[len(prefix):]
			return version.NewSemVer(line)
		}
	}
	return version.SemVer{}, errs.New("unable to determine binary version")
}

func downloadArchive(ctx context.Context, file io.Writer, url string) (err error) {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}

	defer func() { err = errs.Combine(err, resp.Body.Close()) }()

	if resp.StatusCode != http.StatusOK {
		return errs.New("bad status: %s", resp.Status)
	}

	_, err = sync2.Copy(ctx, file, resp.Body)
	return err
}

func unpackBinary(ctx context.Context, archive, target string) (err error) {
	// TODO support different compression types e.g. tar.gz

	zipReader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer func() { err = errs.Combine(err, zipReader.Close()) }()

	if len(zipReader.File) != 1 {
		return errors.New("archive should contain only binary file")
	}

	zipedExec, err := zipReader.File[0].Open()
	if err != nil {
		return err
	}
	defer func() { err = errs.Combine(err, zipedExec.Close()) }()

	newExec, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(0755))
	if err != nil {
		return err
	}
	defer func() { err = errs.Combine(err, newExec.Close()) }()

	_, err = sync2.Copy(ctx, newExec, zipedExec)
	if err != nil {
		return errs.Combine(err, os.Remove(newExec.Name()))
	}
	return nil
}

func restartService(name string) error {
	switch runtime.GOOS {
	case "windows":
		// TODO: combine stdout with err if err
		restartSvcBatPath := filepath.Join(os.TempDir(), "restartservice.bat")
		restartSvcBat, err := os.Create(restartSvcBatPath)
		if err != nil {
			return err
		}

		restartStr := fmt.Sprintf("net stop %s && net start %s", name, name)
		_, err = restartSvcBat.WriteString(restartStr)
		if err != nil {
			return err
		}
		if err := restartSvcBat.Close(); err != nil {
			return err
		}

		_, err = exec.Command(restartSvcBat.Name()).CombinedOutput()
		if err != nil {
			return err
		}
	default:
		return nil
	}
	return nil
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return info.Mode().IsRegular()
}

func main() {
	process.Exec(rootCmd)
}

// TODO: improve logging; other commands use zap but due to an apparent
// windows bug we're unable to use the existing process logging infrastructure.
func openLog() (error, func() error) {
	noop := func() error {return nil}

	if runCfg.Log != "" {
		logFile, err := os.OpenFile(runCfg.Log, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			log.Printf("error opening log file: %s", err)
			return err, noop
		}
		log.Printf("writing all further log output to %s", runCfg.Log)
		log.SetOutput(logFile)
		return nil, logFile.Close
	}
	return nil, noop
}
