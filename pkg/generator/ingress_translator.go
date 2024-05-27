/*
Copyright 2020 The Knative Authors

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

package generator

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"strings"
	"time"

	v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoycorev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	envoymatcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/net-kourier/pkg/bonalib"
	pkgconfig "knative.dev/net-kourier/pkg/config"
	envoy "knative.dev/net-kourier/pkg/envoy/api"
	"knative.dev/net-kourier/pkg/reconciler/ingress/config"
	"knative.dev/net-kourier/pkg/region"
	"knative.dev/networking/pkg/apis/networking/v1alpha1"
	"knative.dev/networking/pkg/certificates"
	netconfig "knative.dev/networking/pkg/config"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/tracker"
)

var _ = bonalib.Baka()

// func percentage(number int) []int {
// 	var a []int
// 	var sum = 0
// 	for i := 1; i <= number; i++ {
// 		var b = rand.IntN(100 - sum)
// 		a = append(a, b)
// 		if i < number {
// 			sum = sum + b
// 		}

// 	}
// 	a[number-1] = 100 - sum
// 	return a
// }

var regions = region.InitRegions()
var mode = region.Mode()
var check = region.Check()

type translatedIngress struct {
	name                    types.NamespacedName
	sniMatches              []*envoy.SNIMatch
	clusters                []*v3.Cluster
	externalVirtualHosts    [][]*route.VirtualHost
	externalTLSVirtualHosts [][]*route.VirtualHost
	internalVirtualHosts    [][]*route.VirtualHost
}

type IngressTranslator struct {
	secretGetter    func(ns, name string) (*corev1.Secret, error)
	endpointsGetter func(ns, name string) (*corev1.Endpoints, error)
	serviceGetter   func(ns, name string) (*corev1.Service, error)
	tracker         tracker.Interface
}

func NewIngressTranslator(
	secretGetter func(ns, name string) (*corev1.Secret, error),
	endpointsGetter func(ns, name string) (*corev1.Endpoints, error),
	serviceGetter func(ns, name string) (*corev1.Service, error),
	tracker tracker.Interface) IngressTranslator {
	return IngressTranslator{
		secretGetter:    secretGetter,
		endpointsGetter: endpointsGetter,
		serviceGetter:   serviceGetter,
		tracker:         tracker,
	}
}

func (translator *IngressTranslator) translateIngress(ctx context.Context, ingress *v1alpha1.Ingress, extAuthzEnabled bool) (*translatedIngress, error) {
	logger := logging.FromContext(ctx)

	sniMatches := make([]*envoy.SNIMatch, 0, len(ingress.Spec.TLS))
	for _, ingressTLS := range ingress.Spec.TLS {
		if err := trackSecret(translator.tracker, ingressTLS.SecretNamespace, ingressTLS.SecretName, ingress); err != nil {
			return nil, err
		}

		secret, err := translator.secretGetter(ingressTLS.SecretNamespace, ingressTLS.SecretName)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch secret: %w", err)
		}

		// Validate certificate here as these are defined by users.
		// We should not send Gateway without validation.
		_, err = tls.X509KeyPair(
			secret.Data[certFieldInSecret],
			secret.Data[keyFieldInSecret],
		)
		if err != nil {
			return nil, fmt.Errorf("invalid secret is specified: %w", err)
		}

		secretRef := types.NamespacedName{
			Namespace: ingressTLS.SecretNamespace,
			Name:      ingressTLS.SecretName,
		}
		sniMatches = append(sniMatches, &envoy.SNIMatch{
			Hosts:            ingressTLS.Hosts,
			CertSource:       secretRef,
			CertificateChain: secret.Data[certFieldInSecret],
			PrivateKey:       secret.Data[keyFieldInSecret]})
	}

	// internalHosts := make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
	// externalHosts := make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
	// externalTLSHosts := make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))

	internalHosts := make([][]*route.VirtualHost, len(regions)+1)
	externalHosts := make([][]*route.VirtualHost, len(regions)+1)
	externalTLSHosts := make([][]*route.VirtualHost, len(regions)+1)

	if mode == 1 {
		internalHosts[0] = make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
		externalHosts[0] = make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
		externalTLSHosts[0] = make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
	} else {
		for i := 0; i < len(regions); i++ {
			internalHosts[i] = make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
			externalHosts[i] = make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
			externalTLSHosts[i] = make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
		}
	}

	clusters := make([]*v3.Cluster, 0, len(ingress.Spec.Rules))

	for i, rule := range ingress.Spec.Rules {
		ruleName := fmt.Sprintf("(%s/%s).Rules[%d]", ingress.Namespace, ingress.Name, i)

		// routes := make([]*route.Route, 0, len(rule.HTTP.Paths))
		routes := make([][]*route.Route, len(regions)+1)
		tlsRoutes := make([]*route.Route, 0, len(rule.HTTP.Paths))
		for _, httpPath := range rule.HTTP.Paths {
			// Default the path to "/" if none is passed.
			path := httpPath.Path
			if path == "" {
				path = "/"
			}

			pathName := fmt.Sprintf("%s.Paths[%s]", ruleName, path)

			// wrs := make([]*route.WeightedCluster_ClusterWeight, 0, len(httpPath.Splits))
			wrs := make([][]*route.WeightedCluster_ClusterWeight, len(regions)+1)

			for _, split := range httpPath.Splits {
				// The FQN of the service is sufficient here, as clusters towards the
				// same service are supposed to be deduplicated anyway.
				splitName := fmt.Sprintf("%s/%s", split.ServiceNamespace, split.ServiceName)

				if err := trackService(translator.tracker, split.ServiceNamespace, split.ServiceName, ingress); err != nil {
					return nil, err
				}

				service, err := translator.serviceGetter(split.ServiceNamespace, split.ServiceName)
				if apierrors.IsNotFound(err) {
					logger.Warnf("Service '%s/%s' not yet created", split.ServiceNamespace, split.ServiceName)
					// TODO(markusthoemmes): Find out if we should actually `continue` here.
					return nil, nil
				} else if err != nil {
					return nil, fmt.Errorf("failed to fetch service '%s/%s': %w", split.ServiceNamespace, split.ServiceName, err)
				}

				// Match the ingress' port with a port on the Service to find the target.
				// Also find out if the target supports HTTP2.
				var (
					externalPort  = int32(80)
					targetPort    = int32(80)
					http2         = false
					httpsPortUsed = false
				)
				for _, port := range service.Spec.Ports {
					if port.Port == split.ServicePort.IntVal || port.Name == split.ServicePort.StrVal {
						externalPort = port.Port
						targetPort = port.TargetPort.IntVal
					}
					if port.Name == "http2" || port.Name == "h2c" {
						http2 = true
					}
					if port.Port == split.ServicePort.IntVal && port.Name == "https" {
						httpsPortUsed = true
					}
				}

				// Disable HTTP2 if the annotation is specified.
				if strings.EqualFold(pkgconfig.GetDisableHTTP2(ingress.Annotations), "true") {
					http2 = false
				}

				var (
					publicLbEndpoints [][]*endpoint.LbEndpoint
					typ               v3.Cluster_DiscoveryType
				)
				if service.Spec.Type == corev1.ServiceTypeExternalName {
					// If the service is of type ExternalName, we add a single endpoint.
					typ = v3.Cluster_LOGICAL_DNS
					publicLbEndpoints[0] = []*endpoint.LbEndpoint{
						envoy.NewLBEndpoint(service.Spec.ExternalName, uint32(externalPort)),
					}
				} else {
					// For all other types, fetch the endpoints object.
					endpoints, err := translator.endpointsGetter(split.ServiceNamespace, split.ServiceName)
					// bonalib.Log("endpoints", len(endpoints.Subsets[0].Addresses))

					if apierrors.IsNotFound(err) {
						logger.Warnf("Endpoints '%s/%s' not yet created", split.ServiceNamespace, split.ServiceName)
						// TODO(markusthoemmes): Find out if we should actually `continue` here.
						return nil, nil
					} else if err != nil {
						return nil, fmt.Errorf("failed to fetch endpoints '%s/%s': %w", split.ServiceNamespace, split.ServiceName, err)
					}

					typ = v3.Cluster_STATIC
					publicLbEndpoints = lbEndpointsForKubeEndpoints(endpoints, targetPort)
				}

				connectTimeout := 5 * time.Second

				var transportSocket *envoycorev3.TransportSocket

				// This has to be "OrDefaults" because this path could be called before the informers are
				// running when booting the controller up and prefilling the config before making it
				// ready.
				//
				// TODO:
				// Drop this configmap check - issues/968
				// We could determine whether system-internal-tls is enabled or disabled via the flag only,
				// but all conformance tests need to be updated to have the port name so we check the configmap as well.
				//
				// TODO: Or fetch configmap before the loop as per https://github.com/knative-sandbox/net-kourier/pull/959#discussion_r1048441513
				cfg := config.FromContextOrDefaults(ctx)

				// As Ingress with RewriteHost points to ExternalService(kourier-internal), we don't enable TLS.
				if (cfg.Network.SystemInternalTLSEnabled() || httpsPortUsed) && httpPath.RewriteHost == "" {
					var err error
					transportSocket, err = translator.createUpstreamTransportSocket(http2, split.ServiceNamespace)
					if err != nil {
						return nil, err
					}
				}

				// fmt.Println("check ne", check)

				if mode == 1 {
					if check == 0 {
						cluster := envoy.NewCluster(splitName, connectTimeout, publicLbEndpoints[0], http2, transportSocket, typ)
						logger.Debugf("adding cluster: %v", cluster)
						clusters = append(clusters, cluster)
						wrs[0] = append(wrs[0], envoy.NewWeightedCluster(splitName, uint32(split.Percent), split.AppendHeaders))
					} else {

						percent := [3]int{1, 50, 49}

						for i := 0; i < len(regions); i++ {
							for key, value := range regions[i] {
								if key == "name" {
									cluster := envoy.NewCluster(splitName+"_"+value, connectTimeout, publicLbEndpoints[i], http2, transportSocket, typ)
									logger.Debugf("adding cluster: %v", cluster)
									clusters = append(clusters, cluster)
									wrs[0] = append(wrs[0], envoy.NewWeightedCluster(splitName+"_"+value, uint32(percent[i]), split.AppendHeaders))
								}
							}
						}
					}
				} else {

					for i := 0; i < len(regions); i++ {
						for key, value := range regions[i] {
							if key == "name" {
								cluster := envoy.NewCluster(splitName+"_"+value, connectTimeout, publicLbEndpoints[i], http2, transportSocket, typ)
								logger.Debugf("adding cluster: %v", cluster)
								clusters = append(clusters, cluster)
								wrs[i] = append(wrs[i], envoy.NewWeightedCluster(splitName+"_"+value, uint32(split.Percent), split.AppendHeaders))
							}
						}
					}
				}

			}

			if len(wrs) != 0 {
				// disable ext_authz filter for HTTP01 challenge when the feature is enabled
				if extAuthzEnabled && strings.HasPrefix(path, "/.well-known/acme-challenge/") {
					routes[0] = append(routes[0], envoy.NewRouteExtAuthzDisabled(
						pathName, matchHeadersFromHTTPPath(httpPath), path, wrs[0], 0, httpPath.AppendHeaders, httpPath.RewriteHost))
				} else if _, ok := os.LookupEnv("KOURIER_HTTPOPTION_DISABLED"); !ok && ingress.Spec.HTTPOption == v1alpha1.HTTPOptionRedirected && rule.Visibility == v1alpha1.IngressVisibilityExternalIP {
					// Do not create redirect route when KOURIER_HTTPOPTION_DISABLED is set. This option is useful when front end proxy handles the redirection.
					// e.g. Kourier on OpenShift handles HTTPOption by OpenShift Route so KOURIER_HTTPOPTION_DISABLED should be set.
					routes[0] = append(routes[0], envoy.NewRedirectRoute(
						pathName, matchHeadersFromHTTPPath(httpPath), path))
				} else {
					// routes = append(routes, envoy.NewRoute(
					// 	pathName, matchHeadersFromHTTPPath(httpPath), path, wrs, 0, httpPath.AppendHeaders, httpPath.RewriteHost))
					if mode == 1 {
						routes[0] = append(routes[0], envoy.NewRoute(
							pathName, matchHeadersFromHTTPPath(httpPath), path, wrs[0], 0, httpPath.AppendHeaders, httpPath.RewriteHost))
					} else {
						for i := 0; i < len(regions); i++ {
							for key, value := range regions[i] {
								if key == "label" {
									routes[i] = append(routes[i], envoy.NewRoute(
										pathName, matchHeadersFromHTTPPath(httpPath), path, wrs[i], 0, httpPath.AppendHeaders, httpPath.RewriteHost, value))
								}
							}
						}
					}
				}
				if len(ingress.Spec.TLS) != 0 || useHTTPSListenerWithOneCert() {
					tlsRoutes = append(tlsRoutes, envoy.NewRoute(
						pathName, matchHeadersFromHTTPPath(httpPath), path, wrs[0], 0, httpPath.AppendHeaders, httpPath.RewriteHost))
				}
			}
		}

		if len(routes) == 0 {
			// Return nothing if there are not routes to generate.
			return nil, nil
		}

		// var virtualHost, virtualTLSHost *route.VirtualHost
		var virtualTLSHost *route.VirtualHost
		virtualHost := make([]*route.VirtualHost, len(regions)+1)

		if extAuthzEnabled {
			contextExtensions := kmeta.UnionMaps(map[string]string{
				"client":     "kourier",
				"visibility": string(rule.Visibility),
			}, ingress.GetLabels())
			virtualHost[0] = envoy.NewVirtualHostWithExtAuthz(ruleName, contextExtensions, domainsForRule(rule), routes[0])
			if len(tlsRoutes) != 0 {
				virtualTLSHost = envoy.NewVirtualHostWithExtAuthz(ruleName, contextExtensions, domainsForRule(rule), tlsRoutes)
			}
		} else {
			// virtualHost = envoy.NewVirtualHost(ruleName, domainsForRule(rule), routes)
			if mode == 1 {
				virtualHost[0] = envoy.NewVirtualHost(ruleName, domainsForRule(rule), routes[0])
			} else {
				for i := 0; i < len(regions); i++ {
					virtualHost[i] = envoy.NewVirtualHost(ruleName, domainsForRule(rule), routes[i])
				}
			}
			if len(tlsRoutes) != 0 {
				virtualTLSHost = envoy.NewVirtualHost(ruleName, domainsForRule(rule), tlsRoutes)
			}
		}

		// internalHosts = append(internalHosts, virtualHost)
		// if rule.Visibility == v1alpha1.IngressVisibilityExternalIP {
		// 	externalHosts = append(externalHosts, virtualHost)
		// 	if virtualTLSHost != nil {
		// 		externalTLSHosts = append(externalTLSHosts, virtualTLSHost)
		// 	}
		// }

		if mode == 1 {
			internalHosts[0] = append(internalHosts[0], virtualHost[0])
			if rule.Visibility == v1alpha1.IngressVisibilityExternalIP {
				externalHosts[0] = append(externalHosts[0], virtualHost[0])
				if virtualTLSHost != nil {
					externalTLSHosts[0] = append(externalTLSHosts[0], virtualTLSHost)
				}
			}
		} else {
			for i := 0; i < len(regions); i++ {
				internalHosts[i] = append(internalHosts[i], virtualHost[i])
				if rule.Visibility == v1alpha1.IngressVisibilityExternalIP {
					externalHosts[i] = append(externalHosts[i], virtualHost[i])
					if virtualTLSHost != nil {
						externalTLSHosts[i] = append(externalTLSHosts[i], virtualTLSHost)
					}
				}
			}
		}

	}

	return &translatedIngress{
		name: types.NamespacedName{
			Namespace: ingress.Namespace,
			Name:      ingress.Name,
		},
		sniMatches:              sniMatches,
		clusters:                clusters,
		externalVirtualHosts:    externalHosts,
		externalTLSVirtualHosts: externalTLSHosts,
		internalVirtualHosts:    internalHosts,
	}, nil
}

func (translator *IngressTranslator) createUpstreamTransportSocket(http2 bool, namespace string) (*envoycorev3.TransportSocket, error) {
	caSecret, err := translator.secretGetter(pkgconfig.ServingNamespace(), netconfig.ServingRoutingCertName)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch activator CA secret: %w", err)
	}
	var alpnProtocols string
	if http2 {
		alpnProtocols = "h2"
	}
	tlsAny, err := anypb.New(createUpstreamTLSContext(caSecret.Data[certificates.CaCertName], namespace, alpnProtocols))
	if err != nil {
		return nil, err
	}
	return &envoycorev3.TransportSocket{
		Name: wellknown.TransportSocketTls,
		ConfigType: &envoycorev3.TransportSocket_TypedConfig{
			TypedConfig: tlsAny,
		},
	}, nil
}

func createUpstreamTLSContext(caCertificate []byte, namespace string, alpnProtocols ...string) *tlsv3.UpstreamTlsContext {
	return &tlsv3.UpstreamTlsContext{
		CommonTlsContext: &tlsv3.CommonTlsContext{
			AlpnProtocols: alpnProtocols,
			TlsParams: &tlsv3.TlsParameters{
				TlsMinimumProtocolVersion: tlsv3.TlsParameters_TLSv1_3,
				TlsMaximumProtocolVersion: tlsv3.TlsParameters_TLSv1_3,
			},
			ValidationContextType: &tlsv3.CommonTlsContext_ValidationContext{
				ValidationContext: &tlsv3.CertificateValidationContext{
					TrustedCa: &envoycorev3.DataSource{
						Specifier: &envoycorev3.DataSource_InlineBytes{
							InlineBytes: caCertificate,
						},
					},
					MatchTypedSubjectAltNames: []*tlsv3.SubjectAltNameMatcher{{
						SanType: tlsv3.SubjectAltNameMatcher_DNS,
						Matcher: &envoymatcherv3.StringMatcher{
							MatchPattern: &envoymatcherv3.StringMatcher_Exact{
								// SAN used by Activator
								Exact: certificates.DataPlaneRoutingSAN,
							},
						},
					}, {
						SanType: tlsv3.SubjectAltNameMatcher_DNS,
						Matcher: &envoymatcherv3.StringMatcher{
							MatchPattern: &envoymatcherv3.StringMatcher_Exact{
								// SAN used by Queue-Proxy in target namespace
								Exact: certificates.DataPlaneUserSAN(namespace),
							},
						},
					}},
				},
			},
		},
	}
}

func trackSecret(t tracker.Interface, ns, name string, ingress *v1alpha1.Ingress) error {
	return t.TrackReference(tracker.Reference{
		Kind:       "Secret",
		APIVersion: "v1",
		Namespace:  ns,
		Name:       name,
	}, ingress)
}

func trackService(t tracker.Interface, svcNs, svcName string, ingress *v1alpha1.Ingress) error {
	if err := t.TrackReference(tracker.Reference{
		Kind:       "Service",
		APIVersion: "v1",
		Namespace:  svcNs,
		Name:       svcName,
	}, ingress); err != nil {
		return fmt.Errorf("could not track service reference: %w", err)
	}

	if err := t.TrackReference(tracker.Reference{
		Kind:       "Endpoints",
		APIVersion: "v1",
		Namespace:  svcNs,
		Name:       svcName,
	}, ingress); err != nil {
		return fmt.Errorf("could not track endpoints reference: %w", err)
	}
	return nil
}

// func lbEndpointsForKubeEndpoints(kubeEndpoints *corev1.Endpoints, targetPort int32) []*endpoint.LbEndpoint {
// 	var readyAddressCount int
// 	for _, subset := range kubeEndpoints.Subsets {
// 		readyAddressCount += len(subset.Addresses)
// 	}

// 	if readyAddressCount == 0 {
// 		return nil
// 	}

// 	eps := make([]*endpoint.LbEndpoint, 0, readyAddressCount)
// 	for _, subset := range kubeEndpoints.Subsets {
// 		for _, address := range subset.Addresses {
// 			eps = append(eps, envoy.NewLBEndpoint(address.IP, uint32(targetPort)))
// 		}
// 	}

// 	return eps
// }

func lbEndpointsForKubeEndpoints(kubeEndpoints *corev1.Endpoints, targetPort int32) [][]*endpoint.LbEndpoint {

	var readyAddressCount int
	for _, subset := range kubeEndpoints.Subsets {
		readyAddressCount += len(subset.Addresses)
	}

	if readyAddressCount == 0 { // bonalog: false
		return nil
	}

	if check == 0 && mode == 1 {
		eps := make([][]*endpoint.LbEndpoint, len(regions)+1)
		for _, subset := range kubeEndpoints.Subsets {
			for _, address := range subset.Addresses {
				eps[0] = append(eps[0], envoy.NewLBEndpoint(address.IP, uint32(targetPort)))
			}
		}
		return eps
	} else {
		eps := make([][]*endpoint.LbEndpoint, len(regions)+1)
		for i := 0; i < len(regions); i++ {
			for key, value := range regions[i] {
				if key == "label" {
					for _, subset := range kubeEndpoints.Subsets {
						for _, address := range subset.Addresses {
							if *address.NodeName == value {
								eps[i] = append(eps[i], envoy.NewLBEndpoint(address.IP, uint32(targetPort)))
							}
						}
					}
				}
			}
		}
		// bonalib.Log("ok", kubeEndpoints.Subsets)
		return eps
	}
	// eps := make([][]*endpoint.LbEndpoint, len(regions)+1)
	// for i := 0; i < len(regions); i++ {
	// 	for key, value := range regions[i] {
	// 		if key == "label" {
	// 			for _, subset := range kubeEndpoints.Subsets {
	// 				for _, address := range subset.Addresses {
	// 					if *address.NodeName == value {
	// 						eps[i] = append(eps[i], envoy.NewLBEndpoint(address.IP, uint32(targetPort)))
	// 					}
	// 				}
	// 			}
	// 		}
	// 	}
	// }
	// return eps
}

func matchHeadersFromHTTPPath(httpPath v1alpha1.HTTPIngressPath) []*route.HeaderMatcher {
	matchHeaders := make([]*route.HeaderMatcher, 0, len(httpPath.Headers))

	for header, matchType := range httpPath.Headers {
		matchHeader := &route.HeaderMatcher{
			Name: header,
		}
		if matchType.Exact != "" {

			matchHeader.HeaderMatchSpecifier = &route.HeaderMatcher_StringMatch{
				StringMatch: &envoymatcherv3.StringMatcher{
					MatchPattern: &envoymatcherv3.StringMatcher_Exact{
						Exact: matchType.Exact,
					},
				},
			}
		}
		matchHeaders = append(matchHeaders, matchHeader)
	}
	return matchHeaders
}

// domainsForRule returns all domains for the given rule.
//
// For example, external domains returns domains with the following formats:
//   - sub-route_host.namespace.example.com
//   - sub-route_host.namespace.example.com:*
//
// Somehow envoy doesn't match properly gRPC authorities with ports.
// The fix is to include ":*" in the domains.
// This applies both for internal and external domains.
// More info https://github.com/envoyproxy/envoy/issues/886
func domainsForRule(rule v1alpha1.IngressRule) []string {
	domains := make([]string, 0, 2*len(rule.Hosts))
	for _, host := range rule.Hosts {
		domains = append(domains, host, host+":*")
	}
	return domains
}
