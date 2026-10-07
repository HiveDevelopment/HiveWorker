package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/api"
	"hivepanel-worker/internal/backup"
	"hivepanel-worker/internal/cell"
	"hivepanel-worker/internal/comb"
	"hivepanel-worker/internal/config"
	"hivepanel-worker/internal/panel"
	hiveruntime "hivepanel-worker/internal/runtime"
	dockerruntime "hivepanel-worker/internal/runtime/docker"
	processruntime "hivepanel-worker/internal/runtime/process"
	workersftp "hivepanel-worker/internal/sftp"
	"hivepanel-worker/internal/updater"

	"golang.org/x/crypto/acme/autocert"
)

func main() {
	cfg := config.Load()

	if len(cfg.Allocations.Entries) == 0 {
		log.Println("No allocations are currently configured")
	}

	allocManager := allocation.NewManager(
		cfg.Allocations.Entries,
	)

	combManager := comb.NewManager(cfg.Paths.Data)
	if err := combManager.Load(); err != nil {
		log.Fatal(err)
	}

	var workerRuntime hiveruntime.Runtime

	switch cfg.Runtime.Type {
	case "process":
		workerRuntime = processruntime.New()

	case "docker":
		dockerRuntime, err := dockerruntime.New(cfg.Docker.Network)
		if err != nil {
			log.Fatal(err)
		}

		workerRuntime = dockerRuntime

	default:
		log.Fatal("unknown runtime type: " + cfg.Runtime.Type)
	}

	backupManager := backup.NewManager(cfg.Paths.Backups)

	backupMountService, err := backup.NewMountService(
		cfg.Paths.Backups,
		cfg.Paths.BackupMounts,
	)
	if err != nil {
		log.Fatal(
			"failed to initialise backup mount service: ",
			err,
		)
	}

	cellManager := cell.NewManager(
		cfg.Paths.Data,
		cfg.Paths.Instances,
		combManager,
		workerRuntime,
		allocManager,
		backupManager,
	)

	if err := cellManager.Load(); err != nil {
		log.Fatal(err)
	}

	if err := cellManager.RecoverRuntime(); err != nil {
		log.Fatal(err)
	}

	panel.StartHeartbeat(cfg)

	if cfg.SFTP.Enabled {
		authClient := panel.NewSFTPAuthClient(cfg)
		handlerFactory := workersftp.NewJailHandlerFactory()

		sftpServer, err := workersftp.NewServer(
			cfg,
			authClient,
			handlerFactory,
		)
		if err != nil {
			log.Fatal("failed to initialise SFTP server: ", err)
		}

		go func() {
			log.Println(
				"HivePanel SFTP server running on " +
					cfg.SFTP.Listen,
			)

			if err := sftpServer.ListenAndServe(); err != nil {
				log.Fatal("SFTP server stopped: ", err)
			}
		}()
	} else {
		log.Println("HivePanel SFTP server is disabled")
	}

	updateManager := updater.NewManager(cfg.Worker.Listen)

	router := api.NewRouter(
		cfg,
		cellManager,
		combManager,
		backupMountService,
		updateManager,
	)

	protocol := "http"
	if cfg.Worker.SSL.Enabled {
		protocol = "https"
	}

	log.Printf(
		"HivePanel Worker running on %s://%s",
		protocol,
		cfg.Worker.Listen,
	)

	log.Println(
		"Config loaded from " +
			cfg.ConfigPath,
	)

	log.Printf(
		"Allocation pool configured with %d exact allocation(s)",
		len(cfg.Allocations.Entries),
	)

	server := &http.Server{
		Addr:    cfg.Worker.Listen,
		Handler: router,
	}

	if cfg.Worker.SSL.Enabled {
		if cfg.Worker.SSL.Auto {
			cacheDir := "/etc/hivepanel/ssl/acme"
			if err := os.MkdirAll(cacheDir, 0700); err != nil {
				log.Fatal("failed to create ACME certificate cache: ", err)
			}

			manager := &autocert.Manager{
				Prompt:     autocert.AcceptTOS,
				Cache:      autocert.DirCache(filepath.Clean(cacheDir)),
				HostPolicy: autocert.HostWhitelist(cfg.Worker.SSL.Hostname),
				Email:      cfg.Worker.SSL.Email,
			}

			challengeServer := &http.Server{
				Addr:    ":80",
				Handler: manager.HTTPHandler(nil),
			}

			go func() {
				log.Printf(
					"HivePanel ACME HTTP-01 challenge server running on :80 for %s",
					cfg.Worker.SSL.Hostname,
				)

				if err := challengeServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Fatal("ACME HTTP-01 challenge server stopped: ", err)
				}
			}()

			server.TLSConfig = manager.TLSConfig()

			log.Printf(
				"HivePanel Worker automatic TLS enabled for %s",
				cfg.Worker.SSL.Hostname,
			)

			log.Fatal(server.ListenAndServeTLS("", ""))
		}

		log.Fatal(
			server.ListenAndServeTLS(
				cfg.Worker.SSL.Cert,
				cfg.Worker.SSL.Key,
			),
		)
	}

	log.Fatal(server.ListenAndServe())
}
