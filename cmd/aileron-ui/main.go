// aileron-ui is a basic, self-hosted web interface for the aileron operator:
// submit VirtualMachineBuild / VirtualMachineClone manifests, watch their
// status, and open VM consoles in the browser. See internal/ui for the
// implementation and the security caveats (it is unauthenticated by design).
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/ui"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	cfg, err := config.GetConfig()
	if err != nil {
		log.Error("kubernetes config unavailable", "error", err)
		os.Exit(1)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error("creating kubernetes client", "error", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error("creating kubernetes clientset", "error", err)
		os.Exit(1)
	}

	namespace := ui.EnvOrDefault("POD_NAMESPACE", "ruddervirt-system")
	gatewayURL := ui.EnvOrDefault("VNCGATEWAY_URL",
		fmt.Sprintf("ws://aileron-vncgateway.%s.svc:7778", namespace))
	addr := ui.EnvOrDefault("LISTEN_ADDR", ":8080")

	srv := ui.NewServer(c, clientset, namespace, gatewayURL, log)

	log.Info("aileron-ui listening", "addr", addr, "namespace", namespace, "vncgateway", gatewayURL)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Error("http server failed", "error", err)
		os.Exit(1)
	}
}
