package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"io/ioutil"
	"log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"

	"github.com/aws/aws-sdk-go/service/eks"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
)

func newClientset(cluster *eks.Cluster, sess *session.Session) (*kubernetes.Clientset, dynamic.Interface, error) {
	log.Printf("%+v", cluster)
	gen, err := token.NewGenerator(true, false)
	if err != nil {
		return nil, nil, err
	}
	opts := &token.GetTokenOptions{
		ClusterID: aws.StringValue(cluster.Name),
		Session:   sess,
	}
	tok, err := gen.GetWithOptions(opts)
	if err != nil {
		return nil, nil, err
	}
	ca, err := base64.StdEncoding.DecodeString(aws.StringValue(cluster.CertificateAuthority.Data))
	if err != nil {
		return nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(
		&rest.Config{
			Host:        aws.StringValue(cluster.Endpoint),
			BearerToken: tok.Token,
			TLSClientConfig: rest.TLSClientConfig{
				CAData: ca,
			},
		},
	)
	dd, err := dynamic.NewForConfig(
		&rest.Config{
			Host:        aws.StringValue(cluster.Endpoint),
			BearerToken: tok.Token,
			TLSClientConfig: rest.TLSClientConfig{
				CAData: ca,
			},
		},
	)
	if err != nil {
		return nil, nil, err
	}
	return clientset, dd, nil
}

func main() {
	name := "eks-emojiapp2"
	region := "us-east-2"
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(region),
	}))
	log.Print(sess.Config.Credentials.Get())
	eksSvc := eks.New(sess)

	input := &eks.DescribeClusterInput{
		Name: aws.String(name),
	}
	result, err := eksSvc.DescribeCluster(input)
	if err != nil {
		log.Fatalf("Error calling DescribeCluster: %v", err)
	}
	clientset, dd, err := newClientset(result.Cluster, sess)
	if err != nil {
		log.Fatalf("Error creating clientset: %v", err)
	}
	nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Fatalf("Error getting EKS nodes: %v", err)
	}
	log.Printf("There are %d nodes associated with cluster %s", len(nodes.Items), name)

	pods, err := clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Fatalf("Error getting pods: %v", err)
	}
	log.Printf("There are %d pods associated with cluster %s", len(pods.Items), name)

	file(clientset, dd)
}

func file(c *kubernetes.Clientset, dd dynamic.Interface) {
	b, err := ioutil.ReadFile("demo.yaml")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%q \n", string(b))

	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(b), 100)

	for {
		var rawObj runtime.RawExtension
		if err = decoder.Decode(&rawObj); err != nil {
			break
		}

		obj, gvk, err := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme).Decode(rawObj.Raw, nil, nil)
		unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			log.Fatal(err)
		}

		unstructuredObj := &unstructured.Unstructured{Object: unstructuredMap}

		gr, err := restmapper.GetAPIGroupResources(c.Discovery())
		if err != nil {
			log.Fatal(err)
		}

		mapper := restmapper.NewDiscoveryRESTMapper(gr)
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			log.Fatal(err)
		}

		var dri dynamic.ResourceInterface

		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			if unstructuredObj.GetNamespace() == "" {
				unstructuredObj.SetNamespace("default")
			}
			dri = dd.Resource(mapping.Resource).Namespace(unstructuredObj.GetNamespace())
		} else {
			dri = dd.Resource(mapping.Resource)
		}

		if _, errUpdate := dri.Update(context.Background(), unstructuredObj, metav1.UpdateOptions{}); errUpdate != nil {
			if _, errCreate := dri.Create(context.Background(), unstructuredObj, metav1.CreateOptions{}); errCreate != nil {
				log.Fatal("In create: ", errCreate)
			}
		}

	}

	if err != io.EOF {
		log.Fatal("eof ", err)
	}
}
