package api

import (
	"context"
	"fmt"
	"hivepanel-worker/internal/allocation"
	"hivepanel-worker/internal/firewall"
	"hivepanel-worker/internal/webproxy"
	"log"
	"net/http"
	"time"

	"hivepanel-worker/internal/auth"
	"hivepanel-worker/internal/backup"
	"hivepanel-worker/internal/cell"
	"hivepanel-worker/internal/comb"
	"hivepanel-worker/internal/config"
	"hivepanel-worker/internal/updater"
)

func NewRouter(cfg config.Config, manager *cell.Manager, combManager *comb.Manager, backupMounts *backup.MountService, updateManager *updater.Manager) http.Handler {
	mux := http.NewServeMux()

	handler := &Handler{
		Config:       cfg,
		Manager:      manager,
		CombManager:  combManager,
		BackupMounts: backupMounts,
		Updater:      updateManager,
	}

	if cfg.Runtime.Type == "docker" && cfg.Paths.Data != "" {
		fw, err := firewall.New(cfg.Paths.Data, nil)
		if err != nil {
			log.Printf("Firewall state could not be loaded: %v", err)
			manager.SetFirewallGuard(func(string, []allocation.Allocation) error {
				return fmt.Errorf("firewall state requires administrator recovery before starting Cells")
			})
		} else {
			handler.Firewall = fw
			go func() {
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for range ticker.C {
					ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
					if err := fw.Restore(ctx); err != nil {
						log.Printf("Firewall reconciliation failed: %v", err)
					}
					cancel()
				}
			}()
			for _, c := range manager.List() {
				_ = fw.Sync(context.Background(), c.ID, append(append([]allocation.Allocation{c.Allocation}, c.AdditionalAllocations...), c.ActiveAllocations...))
			}
			manager.SetFirewallGuard(func(id string, allocs []allocation.Allocation) error {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				return fw.Sync(ctx, id, allocs)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := fw.Restore(ctx); err != nil {
				log.Printf("Firewall restore failed: %v", err)
			}
			cancel()
		}
	}
	if cfg.Runtime.Type == "docker" && cfg.Paths.Data != "" {
		proxy, err := webproxy.New(cfg.Paths.Data, cfg.Docker.Network, cfg.Node.ID, nil)
		if err != nil {
			log.Printf("Node proxy unavailable: %v", err)
		} else {
			handler.WebProxy = proxy
			if handler.Firewall != nil {
				handler.Firewall.OnPolicy = proxy.SetAccessPolicy
				// Reconcile policies after a crash between proxy reload and firewall persistence.
				for _, c := range manager.List() {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					if err := proxy.SetAccessPolicy(ctx, c.ID, handler.Firewall.Policy(c.ID)); err != nil {
						log.Printf("Proxy firewall restore failed for %s: %v", c.ID, err)
					}
					cancel()
				}
			}
			manager.SetDefinitionGuard(proxy.CheckCellPorts)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

				if err := proxy.Restore(ctx); err != nil {
					log.Printf("Node proxy restore failed: %v", err)
				}
				cancel()
				proxy.StartRenewal(context.Background())
			}()
		}
	}
	for pattern, action := range map[string]http.HandlerFunc{
		"GET /configuration/web-proxy": handler.NodeWebProxy, "PATCH /configuration/web-proxy": handler.ConfigureWebProxy,
		"DELETE /configuration/web-proxy/routes/{route}": handler.AdminDeleteDomain,
		"GET /cells/{id}/domains":                        handler.CellDomains, "POST /cells/{id}/domains": handler.SaveDomain,
		"POST /cells/{id}/domains/{route}/verify": handler.VerifyDomain, "POST /cells/{id}/domains/{route}/certificate": handler.DomainCertificate,
		"DELETE /cells/{id}/domains/{route}": handler.DeleteDomain,
	} {
		mux.Handle(pattern, auth.Middleware(cfg, action))
	}
	for pattern, action := range map[string]http.HandlerFunc{
		"POST /cells/{id}/operations/begin": handler.BeginCellOperation, "POST /cells/{id}/operations/end": handler.EndCellOperation,
		"GET /cells/{id}/firewall": handler.CellFirewall, "PUT /cells/{id}/firewall": handler.SetCellFirewall,
		"GET /cells/{id}/instances": handler.CellInstances, "POST /cells/{id}/instances": handler.CreateCellInstance,
		"POST /cells/{id}/instances/{instance}/activate": handler.ActivateCellInstance, "DELETE /cells/{id}/instances/{instance}": handler.DeleteCellInstance,
	} {
		mux.Handle(pattern, auth.Middleware(cfg, action))
	}
	mux.HandleFunc("GET /health", handler.Health)

	mux.HandleFunc("POST /migration/source-detect", handler.DetectMigrationSource)

	mux.Handle("GET /version", auth.Middleware(cfg, http.HandlerFunc(handler.Version)))
	mux.Handle("GET /node/stats", auth.Middleware(cfg, http.HandlerFunc(handler.NodeStats)))

	mux.Handle("POST /update", auth.Middleware(cfg, http.HandlerFunc(handler.UpdateWorker)))
	mux.Handle("GET /update/status", auth.Middleware(cfg, http.HandlerFunc(handler.WorkerUpdateStatus)))

	mux.Handle("PATCH /configuration/allocations", auth.Middleware(cfg, http.HandlerFunc(handler.UpdateAllocationConfiguration)))

	mux.Handle("GET /combs", auth.Middleware(cfg, http.HandlerFunc(handler.ListCombs)))
	mux.Handle("GET /combs/{id}", auth.Middleware(cfg, http.HandlerFunc(handler.GetComb)))

	mux.Handle("GET /cells/overview", auth.Middleware(cfg, http.HandlerFunc(handler.CellOverview)))
	mux.Handle("GET /cells", auth.Middleware(cfg, http.HandlerFunc(handler.ListCells)))
	mux.Handle("POST /cells", auth.Middleware(cfg, http.HandlerFunc(handler.CreateCell)))
	mux.Handle("GET /cells/{id}", auth.Middleware(cfg, http.HandlerFunc(handler.CellStatus)))
	mux.Handle("DELETE /cells/{id}", auth.Middleware(cfg, http.HandlerFunc(handler.DeleteCell)))

	mux.Handle("PATCH /cells/{id}/definition", auth.Middleware(cfg, http.HandlerFunc(handler.UpdateCellDefinition)))

	mux.Handle("POST /cells/{id}/content/install", auth.Middleware(cfg, http.HandlerFunc(handler.InstallContent)))
	mux.Handle("GET /cells/{id}/content/status", auth.Middleware(cfg, http.HandlerFunc(handler.ContentStatus)))

	mux.Handle("POST /cells/{id}/install", auth.Middleware(cfg, http.HandlerFunc(handler.InstallCell)))
	mux.Handle("GET /cells/{id}/install/status", auth.Middleware(cfg, http.HandlerFunc(handler.CellInstallStatus)))
	mux.Handle("POST /cells/{id}/reinstall", auth.Middleware(cfg, http.HandlerFunc(handler.ReinstallCell)))
	mux.Handle("POST /cells/{id}/start", auth.Middleware(cfg, http.HandlerFunc(handler.StartCell)))
	mux.Handle("POST /cells/{id}/network/tcp", auth.Middleware(cfg, http.HandlerFunc(handler.NectarTCP)))
	mux.Handle("POST /cells/{id}/stop", auth.Middleware(cfg, http.HandlerFunc(handler.StopCell)))
	mux.Handle("POST /cells/{id}/command", auth.Middleware(cfg, http.HandlerFunc(handler.SendCommand)))

	mux.Handle("GET /cells/{id}/console", auth.Middleware(cfg, http.HandlerFunc(handler.CellConsole)))
	mux.Handle("POST /cells/{id}/console-session", auth.Middleware(cfg, http.HandlerFunc(handler.CreateConsoleSession)))
	mux.Handle("GET /cells/{id}/stats", auth.Middleware(cfg, http.HandlerFunc(handler.CellStats)))
	mux.Handle("GET /cells/{id}/ws", auth.Middleware(cfg, http.HandlerFunc(handler.ConsoleWebSocket)))

	mux.Handle("POST /cells/{id}/backups", auth.Middleware(cfg, http.HandlerFunc(handler.CreateBackup)))
	mux.Handle("GET /cells/{id}/backups", auth.Middleware(cfg, http.HandlerFunc(handler.ListBackups)))
	mux.Handle("POST /cells/{id}/backups/{name}/mount", auth.Middleware(cfg, http.HandlerFunc(handler.MountBackup)))
	mux.Handle("DELETE /cells/{id}/backup-mounts/{mountId}", auth.Middleware(cfg, http.HandlerFunc(handler.UnmountBackup)))
	mux.Handle("GET /cells/{id}/backup-mounts/{mountId}/files", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.ListMountedBackupFiles))))
	mux.Handle("POST /cells/{id}/backup-mounts/{mountId}/restore", auth.Middleware(cfg, http.HandlerFunc(handler.RestoreMountedBackupPath)))

	mux.Handle("GET /cells/{id}/backups/files", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.ListBackupFiles))))
	mux.Handle("GET /cells/{id}/backups/files/read", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.ReadBackupFile))))
	mux.Handle("POST /cells/{id}/backups/files/extract", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.ExtractBackupFile))))

	mux.Handle("GET /cells/{id}/backups/{backupID}/download", auth.Middleware(cfg, http.HandlerFunc(handler.DownloadBackup)))
	mux.Handle("DELETE /cells/{id}/backups/{backupID}", auth.Middleware(cfg, http.HandlerFunc(handler.DeleteBackup)))
	mux.Handle("POST /cells/{id}/backups/{backupID}/restore", auth.Middleware(cfg, http.HandlerFunc(handler.RestoreBackup)))

	mux.Handle("GET /cells/{id}/files", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.ListFiles))))
	mux.Handle("GET /cells/{id}/files/read", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.ReadFile))))
	mux.Handle("POST /cells/{id}/files/write", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.WriteFile))))
	mux.Handle("DELETE /cells/{id}/files/delete", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.DeleteFile))))
	mux.Handle("POST /cells/{id}/files/folder", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.CreateFolder))))
	mux.Handle("POST /cells/{id}/files/rename", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.RenameFile))))
	mux.Handle("POST /cells/{id}/files/upload", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.UploadFile))))
	mux.Handle("GET /cells/{id}/files/download", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.DownloadFile))))
	mux.Handle("POST /cells/{id}/files/restore", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.RestoreFile))))
	mux.Handle("DELETE /cells/{id}/files/permanent", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.PermanentDeleteFile))))
	mux.Handle("POST /cells/{id}/files/file", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.CreateFile))))
	mux.Handle("POST /cells/{id}/files/upload-url", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.UploadFromURL))))
	mux.Handle("POST /cells/{id}/files/archive", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.CreateFileArchive))))
	mux.Handle("POST /cells/{id}/files/extract", auth.Middleware(cfg, handler.FileLease(http.HandlerFunc(handler.ExtractFileArchive))))

	mux.Handle("GET /cells/{id}/config", auth.Middleware(cfg, http.HandlerFunc(handler.ListConfigFiles)))
	mux.Handle("GET /cells/{id}/config/read", auth.Middleware(cfg, http.HandlerFunc(handler.ReadConfigFile)))
	mux.Handle("PATCH /cells/{id}/config/write", auth.Middleware(cfg, http.HandlerFunc(handler.WriteConfigFile)))

	mux.Handle("GET /cells/{id}/players", auth.Middleware(cfg, http.HandlerFunc(handler.ListPlayers)))
	mux.Handle("POST /cells/{id}/players/kick", auth.Middleware(cfg, http.HandlerFunc(handler.KickPlayer)))
	mux.Handle("POST /cells/{id}/players/ban", auth.Middleware(cfg, http.HandlerFunc(handler.BanPlayer)))
	mux.Handle("POST /cells/{id}/players/op", auth.Middleware(cfg, http.HandlerFunc(handler.OpPlayer)))
	mux.Handle("POST /cells/{id}/players/deop", auth.Middleware(cfg, http.HandlerFunc(handler.DeopPlayer)))

	mux.Handle("POST /cells/{id}/importer/test", auth.Middleware(cfg, http.HandlerFunc(handler.TestImporter)))
	mux.Handle("POST /cells/{id}/importer/start", auth.Middleware(cfg, http.HandlerFunc(handler.StartImporter)))
	mux.Handle("GET /cells/{id}/importer/status", auth.Middleware(cfg, http.HandlerFunc(handler.ImporterStatus)))

	mux.Handle("POST /cells/{id}/lock", auth.Middleware(cfg, http.HandlerFunc(handler.LockCell)))
	mux.Handle("POST /cells/{id}/unlock", auth.Middleware(cfg, http.HandlerFunc(handler.UnlockCell)))

	return mux
}
