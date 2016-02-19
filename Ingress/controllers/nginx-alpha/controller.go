/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"text/template"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/client/record"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	clientcmd "k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util"
)

var (
	argKubecfgFile   = flag.String("kubecfg-file", "", "Location of kubecfg file for access to kubernetes API service; --kube-apiserver-url overrides the URL part of this; if neither this nor --kube-apiserver-url are provided, defaults to service account tokens")
	argKubeMasterURL = flag.String("kube-apiserver-url", "", "URL to reach kubernetes API server. Env variables in this flag will be expanded.")
)

const (
	nginxConfFilePath     = "/etc/nginx/nginx.conf"
	nginxConfFileTestPath = "/etc/nginx/nginx.test.conf"
	nginxConf             = `
events {
  worker_connections 1024;
}
http {
  # http://nginx.org/en/docs/http/ngx_http_core_module.html
  types_hash_max_size 2048;
  server_names_hash_max_size 512;
  server_names_hash_bucket_size 255;

  server {
    listen 80 default_server;
    server_name _;
    return 404;

    location /health {
      access_log off;
      return 200;
    }
  }

{{range $ing := .Items}}
{{range $rule := $ing.Spec.Rules}}
  server {
    {{$annotations := $ing.Annotations}}
    {{if $annotations}}
    {{$client_max_body_size := index $annotations "nginx/client_max_body_size"}}
    {{if $client_max_body_size}}
    client_max_body_size {{$client_max_body_size}};
    {{end}}
    {{end}}

    listen 80;
    server_name {{$rule.Host}};
{{ range $path := $rule.HTTP.Paths }}
    location {{$path.Path}} {
      proxy_set_header Host $host;
      proxy_pass http://{{$path.Backend.ServiceName}}.{{$ing.Namespace}}.svc.cluster.local:{{$path.Backend.ServicePort}};
    }{{end}}
  }{{end}}{{end}}
}`
)

func shellOut(cmd string) {
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil {
		log.Fatalf("Failed to execute %v: %v, err: %v", cmd, string(out), err)
	}
}

func main() {
	flag.Parse()

	var (
		kubeClient       *client.Client
		kubeClientConfig *client.Config
		err              error
	)
	if kubeClientConfig, err = newKubeClientConfig(); err != nil {
		log.Fatalf("Failed to create client config: %v.", err)
	} else if kubeClient, err = client.New(kubeClientConfig); err != nil {
		log.Fatalf("Failed to create kubernetes client: %v.", err)
	}
	updateConfigLoop(kubeClient)
}

func updateConfigLoop(kubeClient *client.Client) {
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("Unable to get hostname: %v", err)
	}
	ingClient := kubeClient.Extensions().Ingress(api.NamespaceAll)
	tmpl, _ := template.New("nginx").Parse(nginxConf)
	rateLimiter := util.NewTokenBucketRateLimiter(0.1, 1)
	known := &extensions.IngressList{}

	pod, err := kubeClient.Pods(api.NamespaceAll).Get(hostname)
	if err != nil {
		log.Fatalf("Unable to get Pod object: %v", err)
	}
	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(api.EventSource{Component: "nginx-alpha-ingress", Host: hostname})
	eventBroadcaster.StartRecordingToSink(kubeClient.Events(""))
	birthCry(pod, recorder)

	shellOut("nginx")
	for {
		rateLimiter.Accept()
		ingresses, err := ingClient.List(labels.Everything(), fields.Everything())
		if err != nil {
			log.Printf("Error retrieving ingresses: %v", err)
			continue
		}
		if reflect.DeepEqual(ingresses.Items, known.Items) {
			continue
		}
		known = ingresses
		if w, err := os.Create(nginxConfFileTestPath); err != nil {
			log.Fatalf("Failed to open %s: %v", nginxConfFileTestPath, err)
		} else if err := tmpl.Execute(w, ingresses); err != nil {
			log.Fatalf("Failed to write template for testing %v", err)
		}
		if err := testConfig(); err != nil {
			reportBadConfig(pod, recorder, err)
			continue
		}
		if err := os.Rename(nginxConfFileTestPath, nginxConfFilePath); err != nil {
			log.Fatalf("Failed to move test template: %v", err)
		}
		shellOut("nginx -s reload")
	}
}

func testConfig() error {
	if _, err := exec.Command("nginx", "-t", "-c", nginxConfFileTestPath).CombinedOutput(); err != nil {
		log.Printf("Error configuring Nginx for ingresses: %v", err)
		return err
	}
	return nil
}

func birthCry(object runtime.Object, recorder record.EventRecorder) {
	recorder.Eventf(object, "Starting", "Starting nginx-alpha ingress controller.")
}

func reportBadConfig(object runtime.Object, recorder record.EventRecorder, err error) {
	recorder.Eventf(object, "Error", "Unable to configure Nginx for ingresses: %s", err)
}

// Lightly adapted from kubernetes/cluster/addons/dns/kube2sky/kube2sky.go
func newKubeClientConfig() (*client.Config, error) {
	var (
		config       *client.Config
		err          error
		apiServerURL string
	)
	// If the user specified --kube-apiserver-url, expand env vars and verify it.
	if *argKubeMasterURL != "" {
		apiServerURL, err = expandKubeMasterURL()
		if err != nil {
			return nil, err
		}
	}

	if apiServerURL != "" && *argKubecfgFile == "" {
		// Only --kube-apiserver-url was provided.
		config = &client.Config{
			Host:    apiServerURL,
			Version: "v1",
		}
	} else {
		// We either have:
		//  1) --kube-apiserver-url and --kubecfg-file
		//  2) just --kubecfg-file
		//  3) neither flag
		// In any case, the logic is the same.  If (3), this will automatically
		// fall back on the service account token.
		overrides := &clientcmd.ConfigOverrides{}
		overrides.ClusterInfo.Server = apiServerURL                                 // might be "", but that is OK
		rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: *argKubecfgFile} // might be "", but that is OK
		if config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig(); err != nil {
			return nil, err
		}
	}

	glog.Infof("Using %s for kubernetes API server", config.Host)
	glog.Infof("Using kubernetes API version %s", config.Version)

	return config, nil
}

func expandKubeMasterURL() (string, error) {
	parsedURL, err := url.Parse(os.ExpandEnv(*argKubeMasterURL))
	if err != nil {
		return "", fmt.Errorf("failed to parse --kube-apiserver-url %s - %v", *argKubeMasterURL, err)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" || parsedURL.Host == ":" {
		return "", fmt.Errorf("invalid --kube-apiserver-url specified %s", *argKubeMasterURL)
	}
	return parsedURL.String(), nil
}
