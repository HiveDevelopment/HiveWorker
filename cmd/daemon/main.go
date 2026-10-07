package main

import (
	"log"
	"net/http"

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

	updateManager := updater.NewManager()

	router := api.NewRouter(
		cfg,
		cellManager,
		combManager,
		backupMountService,
		updateManager,
	)

	log.Println(
		"HivePanel Worker running on " +
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

	log.Fatal(
		http.ListenAndServe(
			cfg.Worker.Listen,
			router,
		),
	)
}
