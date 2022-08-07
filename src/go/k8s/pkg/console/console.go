package console

import (
	"context"
	"fmt"
	"net"

	"github.com/go-logr/logr"
	"github.com/redpanda-data/console/backend/pkg/kafka"
	"github.com/redpanda-data/console/backend/pkg/schema"
	"github.com/redpanda-data/redpanda/src/go/k8s/pkg/resources"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	redpandav1alpha1 "github.com/redpanda-data/redpanda/src/go/k8s/apis/redpanda/v1alpha1"
	labels "github.com/redpanda-data/redpanda/src/go/k8s/pkg/labels"
)

// ConfigMap is a Console resource
type ConfigMap struct {
	client.Client
	scheme     *runtime.Scheme
	consoleobj *redpandav1alpha1.Console
	clusterobj *redpandav1alpha1.Cluster
	log        logr.Logger
}

// NewConfigMap instantiates a new ConfigMap
func NewConfigMap(cl client.Client, scheme *runtime.Scheme, consoleobj *redpandav1alpha1.Console, clusterobj *redpandav1alpha1.Cluster, log logr.Logger) *ConfigMap {
	return &ConfigMap{
		Client:     cl,
		scheme:     scheme,
		consoleobj: consoleobj,
		clusterobj: clusterobj,
		log:        log,
	}
}

// Ensure implements Resource interface
func (cm *ConfigMap) Ensure(ctx context.Context) error {
	config, err := cm.generateConsoleConfig()
	if err != nil {
		return err
	}

	obj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cm.consoleobj.GetName(),
			Namespace: cm.consoleobj.GetNamespace(),
			Labels:    labels.ForConsole(cm.consoleobj),
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		Data: map[string]string{
			"config.yaml": config,
		},
	}

	if err := controllerutil.SetControllerReference(cm.consoleobj, obj, cm.scheme); err != nil {
		return err
	}

	created, err := resources.CreateIfNotExists(ctx, cm.Client, obj, cm.log)
	if err != nil {
		return fmt.Errorf("creating Console configmap: %w", err)
	}

	if !created {
		// Update resource if not created.
		var current corev1.ConfigMap
		err = cm.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, &current)
		if err != nil {
			return fmt.Errorf("fetching Console configmap: %w", err)
		}
		_, err = resources.Update(ctx, &current, obj, cm.Client, cm.log)
		if err != nil {
			return fmt.Errorf("updating Console configmap: %w", err)
		}
	}

	return nil
}

// Key implements Resource interface
func (cm *ConfigMap) Key() types.NamespacedName {
	return types.NamespacedName{Name: cm.consoleobj.GetName(), Namespace: cm.consoleobj.GetNamespace()}
}

func (cm *ConfigMap) generateConsoleConfig() (string, error) {
	consoleConfig := &ConsoleConfig{}
	consoleConfig.SetDefaults()
	consoleConfig.Server = cm.consoleobj.Spec.Server
	consoleConfig.Kafka = cm.genKafka()

	config, err := yaml.Marshal(consoleConfig)
	if err != nil {
		return "", err
	}
	return string(config), nil
}

var (
	// We are using publicly signed certificates for TLS
	// Console is creating a NewCertPool() which will not use host default certificates when left blank
	// REF https://github.com/redpanda-data/console/blob/master/backend/pkg/schema/client.go#L60
	DefaultCAFilePath = "/etc/ssl/certs/ca-certificates.crt"

	SchemaRegistryTLSDir          = "/redpanda/schema-registry"
	SchemaRegistryTLSCertFilePath = fmt.Sprintf("%s/%s", SchemaRegistryTLSDir, "tls.crt")
	SchemaRegistryTLSKeyFilePath  = fmt.Sprintf("%s/%s", SchemaRegistryTLSDir, "tls.key")
)

func (cm *ConfigMap) genKafka() kafka.Config {
	k := kafka.Config{
		Brokers:  getBrokers(cm.clusterobj),
		ClientID: fmt.Sprintf("redpanda-console-%s-%s", cm.consoleobj.GetNamespace(), cm.consoleobj.GetName()),
	}

	schemaRegistry := schema.Config{Enabled: false}
	if yes := cm.consoleobj.Spec.Schema.Enabled; yes {
		tls := schema.TLSConfig{Enabled: false}
		if yes := cm.clusterobj.IsSchemaRegistryTLSEnabled(); yes {
			tls = schema.TLSConfig{
				Enabled:      yes,
				CaFilepath:   DefaultCAFilePath,
				CertFilepath: SchemaRegistryTLSCertFilePath,
				KeyFilepath:  SchemaRegistryTLSKeyFilePath,
			}
		}
		schemaRegistry = schema.Config{Enabled: yes, URLs: []string{cm.clusterobj.SchemaRegistryAPIInternalURL()}, TLS: tls}
	}
	k.Schema = schemaRegistry

	sasl := kafka.SASLConfig{Enabled: false}
	// Set defaults because Console complains SASL mechanism is not set even if SASL is disabled
	sasl.SetDefaults()
	if yes := cm.clusterobj.Spec.EnableSASL; yes {
		sasl = kafka.SASLConfig{
			Enabled:   yes,
			Username:  "",
			Password:  "",
			Mechanism: "SCRAM-SHA-256",
		}
	}
	k.SASL = sasl

	return k
}

func getBrokers(clusterobj *redpandav1alpha1.Cluster) []string {
	if l := clusterobj.InternalListener(); l != nil {
		brokers := []string{}
		for _, host := range clusterobj.Status.Nodes.Internal {
			port := fmt.Sprintf("%d", l.Port)
			brokers = append(brokers, net.JoinHostPort(host, port))
		}
		return brokers
	}
	// External hosts already have ports in them
	return clusterobj.Status.Nodes.External
}
