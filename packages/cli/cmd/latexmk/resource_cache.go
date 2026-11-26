package main

import (
	"context"
	"errors"
	"os"
	"strconv"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/resourcecache"
	"github.com/billstark001/latexmk/packages/shared/argv"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func resolveResourceCache(opts *compileOptions) error {
	directory, err := resourcecache.Directory(opts.resourceCache.Directory, opts.projectRoot)
	if err != nil {
		return err
	}
	opts.resourceCache.Directory = directory
	opts.denyFiles = append(opts.denyFiles, directory)
	return nil
}

func preparePackageArchives(
	ctx context.Context,
	c *client.Client,
	opts compileOptions,
	meta protocol.Metadata,
	environment resources.Environment,
) error {
	if err := opts.resourceCache.Validate(); err != nil {
		return err
	}
	mode := opts.resourceCache.Acquisition
	if mode == "" {
		mode = "auto"
	}
	if mode == "client" && !meta.Capabilities.ResourceArchiveUploads && len(environment.Packages) > 0 &&
		!environment.Ready {
		return &client.CapabilityError{Capability: "client package archive acquisition"}
	}
	if mode == "server" || !meta.Capabilities.ResourceArchiveUploads || len(environment.Packages) == 0 ||
		environment.Ready {
		return nil
	}
	manifest, err := c.PackageArchives(ctx, environment)
	if err != nil {
		return err
	}
	clientFailure := func(err error, pkg resources.Package) error {
		failure := resources.AsAcquisitionError(err, "resource_storage_failed", "archive-cache", "cache", pkg)
		if mode == "auto" && ctx.Err() == nil {
			switch failure.Code {
			case "resource_upstream_network", "resource_upstream_http", "resource_upstream_format",
				"resource_upstream_tls", "resource_archive_checksum", "resource_storage_failed",
				"resource_cache_unavailable", "resource_limit_exceeded":
				terminalFprintf(
					os.Stderr,
					"latexmk: local package acquisition failed (%s); trying server preparation\n",
					failure.Code,
				)
				return nil
			}
		}
		return failure
	}
	var cache *resourcecache.Cache
	defer func() {
		if cache != nil {
			_ = cache.Close()
		}
	}()
	transport := resources.NewArchiveHTTPClient()
	defer transport.CloseIdleConnections()
	for _, item := range manifest.Archives {
		if item.Present {
			continue
		}
		if cache == nil {
			settings := opts.resourceCache
			settings.Directory, err = resourcecache.Directory(settings.Directory, opts.projectRoot)
			if err != nil {
				return err
			}
			cache, err = resourcecache.Open(settings)
			if err != nil {
				return clientFailure(err, item.Package)
			}
		}
		archive, err := cache.Acquire(ctx, resources.ArchiveFetchOptions{
			Client: transport, Repository: environment.Baseline.Repository,
			Package: item.Package, Policy: manifest.Sources, MaxBytes: item.MaxBytes,
		})
		if err != nil {
			return clientFailure(err, item.Package)
		}
		uploadErr := c.UploadPackageArchive(ctx, environment.Digest, item.Package, archive.File, archive.Size)
		if err := errors.Join(uploadErr, archive.Close()); err != nil {
			return err
		}
	}
	return nil
}

func runResourceCache(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return failAgent("cache.resources", false, err)
	}
	type options struct {
		settings resourcecache.Settings
		json     bool
		all      bool
	}
	changes := new(argv.Options[options])
	changes.Bool("json", func(o *options, value bool) error { o.json = value; return nil })
	changes.Bool("all", func(o *options, value bool) error { o.all = value; return nil })
	changes.Value(
		"resource-cache-dir",
		func(o *options, value string) error { o.settings.Directory = value; return nil },
	)
	changes.Value("resource-cache-bytes", func(o *options, value string) error {
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || limit < 1 || limit > 1<<40 {
			return errors.New("--resource-cache-bytes requires a positive byte count at most 1099511627776")
		}
		o.settings.MaxBytes = limit
		return nil
	})
	cfg, parsed, err := loadCommand("cache.resources", cwd, args, changes.Flags()...)
	jsonOutput := parsed != nil && parsed.Bool("json")
	if err != nil {
		return failAgentArguments("cache.resources", jsonOutput, err)
	}
	positionals := parsed.Args().Slice()
	if len(positionals) != 1 || (positionals[0] != "info" && positionals[0] != "prune") {
		return failAgentArguments("cache.resources", jsonOutput, errors.New("cache resources supports info and prune"))
	}
	opts := options{settings: cfg.ResourceCache}
	if err := changes.Apply(&opts); err != nil {
		return failAgentArguments("cache.resources", jsonOutput, err)
	}
	if opts.all && positionals[0] != "prune" {
		return failAgentArguments("cache.resources", jsonOutput, errors.New("--all requires cache resources prune"))
	}
	directory, err := resourcecache.Directory(opts.settings.Directory, cwd)
	if err != nil {
		return failAgentArguments("cache.resources", jsonOutput, err)
	}
	opts.settings.Directory = directory
	cache, err := resourcecache.Open(opts.settings)
	if err != nil {
		return failAgent("cache.resources", jsonOutput, err)
	}
	defer func() { _ = cache.Close() }()
	var info resourcecache.Info
	if positionals[0] == "prune" {
		info, err = cache.Prune(context.Background(), opts.all)
	} else {
		info, err = cache.Info()
	}
	if err != nil {
		return failAgent("cache.resources."+positionals[0], jsonOutput, err)
	}
	if jsonOutput {
		if err := writeAgentJSON("cache.resources."+positionals[0], info); err != nil {
			return fail(err)
		}
	} else {
		terminalPrintf(
			"resource cache: %s\n%d archives, %d / %d bytes\n",
			info.Directory,
			info.Archives,
			info.Bytes,
			info.MaxBytes,
		)
	}
	return 0
}
