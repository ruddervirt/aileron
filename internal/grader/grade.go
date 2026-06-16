package grader

import (
	"fmt"
	"net/http"
	"sync"

	gwebsocket "github.com/gorilla/websocket"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	transportwebsocket "k8s.io/client-go/transport/websocket"

	"github.com/ruddervirt/aileron/internal/ws"
)

var gradeVMLocks sync.Map

func Grade(method GradeMethod, namespace, vmName, username, password, domain string, commands []string) ([]ws.CommandResult, error) {
	lockKey := namespace + "/" + vmName
	mu, _ := gradeVMLocks.LoadOrStore(lockKey, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	config.GroupVersion = &schema.GroupVersion{Group: "subresources.kubevirt.io", Version: "v1"}
	config.APIPath = "/apis"
	config.NegotiatedSerializer = runtime.NewSimpleNegotiatedSerializer(runtime.SerializerInfo{})

	restClient, err := rest.RESTClientFor(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST client: %w", err)
	}

	switch method {
	case GradeMethodSerialWindows:
		return gradeSerial(restClient, config, namespace, vmName, username, password, domain, commands, ws.RunCommandsWithRudderGrade)
	case GradeMethodSerialLinux:
		return gradeSerial(restClient, config, namespace, vmName, username, password, domain, commands, ws.RunCommandsWithRudderGradeLinux)
	default:
		return nil, fmt.Errorf("unsupported grade method %s", method)
	}
}

func gradeSerial(restClient *rest.RESTClient, config *rest.Config, namespace, vmName, username, password, domain string, commands []string, runner func(*gwebsocket.Conn, string, string, string, []string) ([]ws.CommandResult, error)) ([]ws.CommandResult, error) {
	uri := fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachineinstances/%s/console", namespace, vmName)

	req := restClient.Get().RequestURI(uri).URL()

	roundtripper, connholder, err := transportwebsocket.RoundTripperFor(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create roundtripper: %w", err)
	}

	httpReq, err := http.NewRequest("GET", req.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	wsConn, err := transportwebsocket.Negotiate(roundtripper, connholder, httpReq, "plain.kubevirt.io")
	if err != nil {
		return nil, fmt.Errorf("failed to negotiate websocket connection: %w", err)
	}

	results, err := runner(wsConn, username, password, domain, commands)
	if err != nil {
		return nil, fmt.Errorf("failed to run commands: %w", err)
	}

	return results, nil
}
