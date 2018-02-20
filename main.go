package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/ericchiang/k8s"
	apiv1 "github.com/ericchiang/k8s/api/v1"
	"github.com/ghodss/yaml"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"os"
	"os/exec"
	"reflect"
	"text/template"
	"time"
)

var tmplFile string
var configFile string
var reloadScript string
var syncPeriod int

var log = logrus.New()

type Service struct {
	Name           string
	Namespace      string
	Endpoints      []string
	BackendPort    int32
	FrontendPort   int32
	LoadBalancerIP string
}

func loadClient(kubeconfigPath string) (*k8s.Client, error) {

	data, err := ioutil.ReadFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %v", err)
	}

	var config k8s.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("unmarshal kubeconfig: %v", err)
	}

	return k8s.NewClient(&config)
}

func getServiceEndpoints(client *k8s.Client, name string, namespace string, servicePort *apiv1.ServicePort) (endpoints []string) {

	ep, _ := client.CoreV1().GetEndpoints(context.Background(), name, namespace)

	if *ep.Metadata.Name == name && *ep.Metadata.Namespace == namespace {
		for _, ss := range ep.Subsets {
			var targetPort int32
			for _, epPort := range ss.Ports {
				if *epPort.Port == servicePort.TargetPort.GetIntVal() {
					targetPort = *epPort.Port
				}
			}
			if targetPort == 0 {
				continue
			}
			for _, epAddress := range ss.Addresses {
				endpoints = append(endpoints, fmt.Sprintf("%v:%v", *epAddress.Ip, targetPort))
			}

		}
		log.Debugf(" -> Found Endpoints: %v\n", endpoints)
	}

	return
}

func getServiceNameForLBRule(s *apiv1.Service, servicePort int32) string {
	return fmt.Sprintf("%v_%v_%v", *s.Metadata.Namespace, *s.Metadata.Name, servicePort)
}

func getServices(client *k8s.Client) (services []Service) {

	svcs, _ := client.CoreV1().ListServices(context.Background(), k8s.AllNamespaces)

	for _, s := range svcs.Items {

		log.Debugf("Service Candidate : %v:%+v type=%+v", *s.Metadata.Namespace, *s.Metadata.Name, *s.Spec.Type)

		if *s.Spec.Type != "LoadBalancer" {
			log.Debugf(" - Dropped candidate : %+v, not loadbalancer type", *s.Metadata.Name)
			continue
		}

		if *s.Spec.LoadBalancerIP == "" {
			log.Debugf(" - Dropped candidate : %+v, no loadbalancer IP", *s.Metadata.Name)
			continue
		}

		for _, servicePort := range s.Spec.Ports {

			ep := getServiceEndpoints(client, *s.Metadata.Name, *s.Metadata.Namespace, servicePort)

			if len(ep) == 0 {
				log.Debugf(" - No endpoints found for service %v, port %v", *s.Metadata.Name, servicePort)
				log.Debugf(" - Dropped candidate : %+v", *s.Metadata.Name)
				continue
			}

			cService := Service{
				Name:           getServiceNameForLBRule(s, *servicePort.Port),
				Endpoints:      ep,
				BackendPort:    *servicePort.Port,
				FrontendPort:   *servicePort.Port,
				LoadBalancerIP: *s.Spec.LoadBalancerIP,
			}

			services = append(services, cService)

			log.Debugf("Candidate OK : %+v", cService)
		}
	}

	return
}

func configureServices(services []Service, tmplFile string, configFile string) {

	for n, service := range services {
		log.Infof("-+= Service #%v", n)
		log.Infof(" |--= Name : %v", service.Name)
		log.Infof(" |--= Endpoints : %v", service.Endpoints)
		log.Infof(" |--= BackendPort : %v", service.BackendPort)
		log.Infof(" |--= FrontendPort : %v", service.FrontendPort)
		log.Infof(" `--= LoadBalancerIP : %v", service.LoadBalancerIP)
	}

	t, err := template.ParseFiles(tmplFile)
	if err != nil {
		log.Errorf("Failed to load template file: %v", err)
		return
	}

	w, err := os.Create(configFile)
	if err != nil {
		log.Errorf("Failed to open config file: %v", err)
		return
	}

	conf := make(map[string]interface{})
	conf["services"] = services

	err = t.Execute(w, conf)
	if err != nil {
		log.Errorf("Failed to write config file: %v", err)
		return
	} else {
		log.Infof("Write config file: %v", configFile)
	}

	log.Infof("Ready to reload proxy")

	out, err := exec.Command(reloadScript).CombinedOutput()
	if err != nil {
		log.Errorf("Error reloading proxy: %v\n%s", err, out)
	} else {
		log.Infof("Reload script succeed:\n%s", out)
	}

	return
}

func init() {

	flag.StringVar(&tmplFile, "tmplFile", "config.tmpl", "Template file to load")
	flag.StringVar(&configFile, "configFile", "config.conf", "Configuration file to write")
	flag.StringVar(&reloadScript, "reloadScript", "./reload.sh", "Reload script to launch")
	flag.IntVar(&syncPeriod, "syncPeriod", 10, "Period between update")

	log.Formatter = new(logrus.TextFormatter)
	log.Level = logrus.InfoLevel
}

func main() {

	flag.Parse()

	clientConfig := os.Getenv("HOME") + "/.kube/config"
	client, err := loadClient(clientConfig)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	log.Infof("Initial GetServices fired")
	currentServices := getServices(client)
	configureServices(currentServices, tmplFile, configFile)

	for t := range time.NewTicker(time.Duration(syncPeriod) * time.Second).C {

		log.Infof("GetServices fired at %+v", t)
		newServices := getServices(client)

		if !reflect.DeepEqual(newServices, currentServices) {
			log.Infof("Services have changed, reload fired")
			currentServices = newServices
			configureServices(currentServices, tmplFile, configFile)
		}
	}
}
