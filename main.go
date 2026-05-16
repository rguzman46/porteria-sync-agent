// porteria-sync-agent — sync agent del módulo LPR de Porteria Plus.
//
// Funciones:
//   - Polling al cloud (/api/access/whitelist) cada N segundos.
//   - Push de placas autorizadas a la cámara LPR local via su API nativa.
//   - Heartbeat para que el cloud detecte caídas del agent.
//   - Self-install como Windows Service / launchd / systemd.
//
// Filosofía:
//   - La portería NUNCA depende del cloud para abrir. La cámara YA tiene
//     la whitelist sincronizada y decide localmente.
//   - El cloud es la fuente de verdad — cuando vuelve, el agent re-sincroniza.
//   - Operación offline-first: si el cloud está caído por horas, el último
//     whitelist sincronizado queda válido y la portería opera normal.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/kardianos/service"
)

const (
	ServiceName        = "PorteriaSyncAgent"
	ServiceDisplayName = "Porteria Plus — Sync Agent"
	ServiceDescription = "Sincroniza placas autorizadas con la cámara LPR local del conjunto."
)

// program implementa service.Interface — la API estándar de kardianos/service
// para registrarse como Windows Service / launchd / systemd.
type program struct {
	cfg    *Config
	cancel context.CancelFunc
}

func (p *program) Start(s service.Service) error {
	// Start no debe bloquear — la lógica corre en goroutine separada.
	go p.run()
	return nil
}

func (p *program) Stop(s service.Service) error {
	log.Println("[main] stop solicitado por el sistema")
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

func (p *program) run() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	defer cancel()

	log.Printf("[main] arrancando sync agent v%s (cloud=%s, camera=%s @ %s, receiver=%v)",
		AgentVersion, p.cfg.Cloud.BaseURL, p.cfg.Camera.Type, p.cfg.Camera.Host, p.cfg.Receiver.Enabled)

	// 1) Construir cliente cloud (compartido entre syncer y replay).
	cloud := NewCloudClient(p.cfg.Cloud.BaseURL, p.cfg.Cloud.Token)

	// 2) Construir adapter de cámara (para whitelist push).
	camera, err := NewCameraAdapter(p.cfg)
	if err != nil {
		log.Fatalf("[main] error construyendo camera adapter: %v", err)
	}

	// 3) Ping inicial — fallar rápido si algo está mal configurado.
	pingCtx, pingCancel := context.WithCancel(ctx)
	if err := camera.Ping(pingCtx); err != nil {
		log.Printf("[main] ⚠ ping a cámara falló: %v (continúo de todas formas — la cámara puede estar arrancando)", err)
	}
	pingCancel()

	// 4) Si Receiver.Enabled, arrancar HTTP receiver + replay worker en
	// goroutines separadas. La captura visual es opt-in vía config — los
	// agents v0.1.x existentes (sin sección `receiver:` en yaml) siguen
	// corriendo solo como puller (whitelist + heartbeat) sin cambios.
	if p.cfg.Receiver.Enabled {
		queue, err := NewFileQueue(p.cfg.Receiver.QueueDir, p.cfg.Receiver.MaxQueueItems, p.cfg.Receiver.MaxQueueBytes)
		if err != nil {
			log.Fatalf("[main] error inicializando queue local: %v", err)
		}

		receiver := NewReceiver(p.cfg, queue)
		go func() {
			if err := receiver.Start(ctx); err != nil {
				log.Printf("[main] receiver terminó con error: %v", err)
			}
		}()

		replay := NewReplayWorker(cloud, queue, p.cfg.Receiver.ReplayTickSec)
		go replay.Run(ctx)

		items, bytes, oldest := queue.Stats()
		log.Printf("[main] receiver+replay activos. Queue actual: items=%d bytes=%d oldest=%s",
			items, bytes, oldest.Round(time.Second))
	} else {
		log.Println("[main] receiver deshabilitado (modo legacy v0.1.x — solo whitelist sync)")
	}

	// 5) Arrancar el loop de sync (whitelist + heartbeat + auto-config).
	syncer := NewSyncer(cloud, p.cfg, camera, p.cfg.Poll.IntervalSeconds)
	syncer.Run(ctx)
}

func main() {
	var (
		configPath  = flag.String("config", "", "Ruta al config.yaml (default: junto al binario)")
		installFlag = flag.Bool("install", false, "Instalar como servicio del sistema")
		uninstallFlag = flag.Bool("uninstall", false, "Desinstalar el servicio del sistema")
		startFlag   = flag.Bool("start", false, "Arrancar el servicio (si está instalado)")
		stopFlag    = flag.Bool("stop", false, "Detener el servicio (si está corriendo)")
		statusFlag  = flag.Bool("status", false, "Mostrar el estado del servicio")
		versionFlag = flag.Bool("version", false, "Mostrar la versión del agent")
	)
	flag.Parse()

	if *versionFlag {
		fmt.Printf("porteria-sync-agent v%s\n", AgentVersion)
		return
	}

	cfg, absConfigPath, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("error cargando config: %v", err)
	}

	// El servicio del sistema corre con el binario en una ruta absoluta.
	// Pasamos el config path resuelto como argumento para que el daemon
	// la cargue desde el mismo lugar tras un reboot del PC.
	exePath, _ := os.Executable()
	svcConfig := &service.Config{
		Name:        ServiceName,
		DisplayName: ServiceDisplayName,
		Description: ServiceDescription,
		Executable:  exePath,
		Arguments:   []string{"-config", absConfigPath},
	}

	prg := &program{cfg: cfg}
	svc, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatalf("error construyendo servicio: %v", err)
	}

	switch {
	case *installFlag:
		if err := svc.Install(); err != nil {
			log.Fatalf("install: %v", err)
		}
		fmt.Println("Servicio instalado. Usa -start para arrancarlo.")
		return
	case *uninstallFlag:
		_ = svc.Stop()
		if err := svc.Uninstall(); err != nil {
			log.Fatalf("uninstall: %v", err)
		}
		fmt.Println("Servicio desinstalado.")
		return
	case *startFlag:
		if err := svc.Start(); err != nil {
			log.Fatalf("start: %v", err)
		}
		fmt.Println("Servicio arrancado.")
		return
	case *stopFlag:
		if err := svc.Stop(); err != nil {
			log.Fatalf("stop: %v", err)
		}
		fmt.Println("Servicio detenido.")
		return
	case *statusFlag:
		st, err := svc.Status()
		if err != nil {
			log.Fatalf("status: %v", err)
		}
		fmt.Printf("Estado: %s\n", statusString(st))
		return
	}

	// Sin flags de gestión: corremos en foreground (modo desarrollo o
	// el propio Windows Service nos invoca sin flags).
	if err := svc.Run(); err != nil {
		log.Fatalf("service.Run: %v", err)
	}
}

func statusString(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}
