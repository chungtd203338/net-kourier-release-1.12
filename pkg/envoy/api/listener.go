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

package envoy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	_ "net/http/pprof"

	cryptomb "github.com/envoyproxy/go-control-plane/contrib/envoy/extensions/private_key_providers/cryptomb/v3alpha"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	prx "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/proxy_protocol/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	auth "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"knative.dev/net-kourier/pkg/bonalib"
	"knative.dev/net-kourier/pkg/config"
	"knative.dev/net-kourier/pkg/region"
)

var _ = bonalib.Baka()
var regions = region.InitRegions()
var mode = region.Mode()

type IPAMBlock struct {
	Spec struct {
		CIDR     string  `json:"cidr"`
		Affinity *string `json:"affinity"`
	} `json:"spec"`
}

type IPAMBlockList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPAMBlock `json:"items"`
}

func (obj *IPAMBlockList) DeepCopyObject() runtime.Object {
	dst := &IPAMBlockList{}
	dst.TypeMeta = obj.TypeMeta
	dst.ListMeta = obj.ListMeta
	dst.Items = make([]IPAMBlock, len(obj.Items))
	for i := range obj.Items {
		dst.Items[i] = obj.Items[i]
	}
	return dst
}

// SNIMatch represents an SNI match, including the hosts to match, the certificates and
// keys to use and the source where we got the certs/keys from.
type SNIMatch struct {
	Hosts            []string
	CertSource       types.NamespacedName
	CertificateChain []byte
	PrivateKey       []byte
}

// NewHTTPListener creates a new Listener at the given port, backed by the given manager.
func NewHTTPListener(manager []*hcm.HttpConnectionManager, port uint32, enableProxyProtocol bool) (*listener.Listener, error) {
	// filters, err := createFilters(manager)
	// if err != nil {
	// 	return nil, err
	// }

	filters := make([][]*listener.Filter, len(regions)+1)
	var err error

	if mode == 1 {
		filters[0], err = createFilters(manager[0])
		if err != nil {
			return nil, err
		}
	} else {
		for i := 0; i < len(regions); i++ {
			filters[i], err = createFilters(manager[i])
			if err != nil {
				return nil, err
			}
		}
	}

	var listenerFilter []*listener.ListenerFilter
	if enableProxyProtocol {
		proxyProtocolListenerFilter, err := createProxyProtocolListenerFilter()
		if err != nil {
			return nil, err
		}
		listenerFilter = append(listenerFilter, proxyProtocolListenerFilter)
	}

	// return &listener.Listener{
	// 	Name:            CreateListenerName(port),
	// 	Address:         createAddress(port),
	// 	ListenerFilters: listenerFilter,
	// 	FilterChains: []*listener.FilterChain{{
	// 		Filters: filters,
	// 	}},
	// }, nil
	_listener := &listener.Listener{
		Name:            CreateListenerName(port),
		Address:         createAddress(port),
		ListenerFilters: listenerFilter,
		FilterChains:    []*listener.FilterChain{},
	}

	if mode == 1 {
		_listener.FilterChains = append(_listener.FilterChains, &listener.FilterChain{
			Filters: filters[0],
		})
	} else {
		// home := homedir.HomeDir()
		// config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(home, ".kube", "config"))
		config, err := rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err.Error())
		}

		nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			panic(err)
		}
		var ipamBlockList IPAMBlockList
		filterChainMatch := make([]*listener.FilterChainMatch, len(regions)+1)

		retry.OnError(retry.DefaultRetry, func(error) bool { return true }, func() error {
			return clientset.RESTClient().
				Get().
				AbsPath("/apis/crd.projectcalico.org/v1/ipamblocks").
				Do(context.TODO()).
				Into(&ipamBlockList)
		})
		nodeip := []corev1.NodeAddress{}
		for i := 0; i < len(regions); i++ {
			filterChainMatch[i] = &listener.FilterChainMatch{
				SourceType:         0,
				SourcePrefixRanges: []*core.CidrRange{},
			}
			for key, value := range regions[i] {
				if key == "label" {
					var numberNodes = 0
					for j := 0; j < len(nodes.Items); j++ {
						if nodes.Items[j].Name == value {
							numberNodes = numberNodes + 1
							nodeip = nodes.Items[j].Status.Addresses
						}
					}
					for k := 0; k < numberNodes; k++ {
						filterChainMatch[i].SourcePrefixRanges = append(filterChainMatch[i].SourcePrefixRanges, &core.CidrRange{
							AddressPrefix: nodeip[k].Address,
							PrefixLen:     wrapperspb.UInt32(32),
						})
					}
					for _, item := range ipamBlockList.Items {
						if item.Spec.Affinity != nil && strings.Contains(*item.Spec.Affinity, value) {
							ip, ipNet, err := net.ParseCIDR(item.Spec.CIDR)
							ones, _ := ipNet.Mask.Size()
							if err != nil {
								fmt.Println("Lỗi khi phân tích CIDR:", err)
							}
							filterChainMatch[i].SourcePrefixRanges = append(filterChainMatch[i].SourcePrefixRanges, &core.CidrRange{
								AddressPrefix: ip.String(),
								PrefixLen:     wrapperspb.UInt32(uint32(ones)),
							})
						}
					}
				}
			}
			_listener.FilterChains = append(_listener.FilterChains, &listener.FilterChain{
				FilterChainMatch: filterChainMatch[i],
				Filters:          filters[i],
			})
		}
	}
	// bonalib.Log("listener", _listener)
	return _listener, nil
}

// NewHTTPSListener creates a new Listener at the given port with a given filter chain
func NewHTTPSListener(port uint32, filterChain []*listener.FilterChain, enableProxyProtocol bool) (*listener.Listener, error) {
	var listenerFilter []*listener.ListenerFilter
	if enableProxyProtocol {
		proxyProtocolListenerFilter, err := createProxyProtocolListenerFilter()
		if err != nil {
			return nil, err
		}
		listenerFilter = append(listenerFilter, proxyProtocolListenerFilter)
	}

	return &listener.Listener{
		Name:            CreateListenerName(port),
		Address:         createAddress(port),
		ListenerFilters: listenerFilter,
		FilterChains:    filterChain,
	}, nil
}

// CreateFilterChainFromCertificateAndPrivateKey creates a new filter chain from a certificate and a private key
func CreateFilterChainFromCertificateAndPrivateKey(
	manager *hcm.HttpConnectionManager,
	cert *Certificate) (*listener.FilterChain, error) {

	filters, err := createFilters(manager)
	if err != nil {
		return nil, err
	}

	tlsContext, err := cert.createTLSContext()
	if err != nil {
		return nil, err
	}
	tlsAny, err := anypb.New(tlsContext)
	if err != nil {
		return nil, err
	}

	return &listener.FilterChain{
		Filters: filters,
		TransportSocket: &core.TransportSocket{
			Name:       wellknown.TransportSocketTls,
			ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: tlsAny},
		},
	}, nil
}

// NewHTTPSListenerWithSNI creates a new Listener at the given port, backed by the given
// manager and applies a FilterChain with the given sniMatches.
//
// Ref: https://www.envoyproxy.io/docs/envoy/latest/faq/configuration/sni.html
func NewHTTPSListenerWithSNI(manager *hcm.HttpConnectionManager, port uint32, sniMatches []*SNIMatch, kourierConfig *config.Kourier) (*listener.Listener, error) {
	filterChains, err := createFilterChainsForTLS(manager, sniMatches, kourierConfig)
	if err != nil {
		return nil, err
	}

	var listenerFilter []*listener.ListenerFilter

	// proxy protocol listener filter should be executed before the TLS inspector listener filter.
	// Since the proxy protocol adds bytes to the beginning of the connection,
	// the SNI will not be parsed correctly if the proxy protocol listener filter is not executed first.
	// Without SNI matching, you would get the wrong certificate, and traffic would drop.
	// https://github.com/solo-io/gloo/issues/5116
	if kourierConfig.EnableProxyProtocol {
		proxyProtocolListenerFilter, err := createProxyProtocolListenerFilter()
		if err != nil {
			return nil, err
		}
		listenerFilter = append(listenerFilter, proxyProtocolListenerFilter)
	}

	listenerFilterForTLS := &listener.ListenerFilter{
		// TLS Inspector listener filter must be configured in order to
		// detect requested SNI.
		// Ref: https://www.envoyproxy.io/docs/envoy/latest/faq/configuration/sni.html
		Name: wellknown.TlsInspector,
		ConfigType: &listener.ListenerFilter_TypedConfig{TypedConfig: &anypb.Any{
			TypeUrl: "type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector",
		}},
	}

	listenerFilter = append(listenerFilter, listenerFilterForTLS)

	return &listener.Listener{
		Name:            CreateListenerName(port),
		Address:         createAddress(port),
		FilterChains:    filterChains,
		ListenerFilters: listenerFilter,
	}, nil
}

// CreateListenerName returns a listener name based on port
func CreateListenerName(port uint32) string {
	return fmt.Sprintf("listener_%d", port)
}

func createAddress(port uint32) *core.Address {
	return &core.Address{
		Address: &core.Address_SocketAddress{
			SocketAddress: &core.SocketAddress{
				Protocol: core.SocketAddress_TCP,
				Address:  "0.0.0.0",
				PortSpecifier: &core.SocketAddress_PortValue{
					PortValue: port,
				},
			},
		},
	}
}

func createFilters(manager *hcm.HttpConnectionManager) ([]*listener.Filter, error) {
	managerAny, err := anypb.New(manager)
	if err != nil {
		return nil, err
	}

	return []*listener.Filter{{
		Name:       wellknown.HTTPConnectionManager,
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: managerAny},
	}}, nil
}

func createFilterChainsForTLS(manager *hcm.HttpConnectionManager, sniMatches []*SNIMatch, kourierConfig *config.Kourier) ([]*listener.FilterChain, error) {
	res := make([]*listener.FilterChain, 0, len(sniMatches))
	for _, sniMatch := range sniMatches {
		filters, err := createFilters(manager)
		if err != nil {
			return nil, err
		}

		c := Certificate{Certificate: sniMatch.CertificateChain, PrivateKey: sniMatch.PrivateKey, CipherSuites: kourierConfig.CipherSuites.List()}

		tlsContext, err := c.createTLSContext()
		if err != nil {
			return nil, err
		}
		tlsAny, err := anypb.New(tlsContext)
		if err != nil {
			return nil, err
		}

		filterChain := listener.FilterChain{
			FilterChainMatch: &listener.FilterChainMatch{
				ServerNames: sniMatch.Hosts,
			},
			TransportSocket: &core.TransportSocket{
				Name:       wellknown.TransportSocketTls,
				ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: tlsAny},
			},
			Filters: filters,
		}

		res = append(res, &filterChain)
	}

	return res, nil
}

// Certificate stores certificate data to generrate TLS context for downstream.
type Certificate struct {
	Certificate        []byte
	PrivateKey         []byte
	PrivateKeyProvider string
	PollDelay          *durationpb.Duration
	CipherSuites       []string
}

// messageToAny converts from proto message to proto Any
func messageToAny(msg proto.Message) (*anypb.Any, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		err = fmt.Errorf("error marshaling message %s: %w", prototext.Format(msg), err)
		return nil, err
	}
	return &anypb.Any{
		// nolint: staticcheck
		TypeUrl: "type.googleapis.com/" + string(msg.ProtoReflect().Descriptor().FullName()),
		Value:   b,
	}, nil
}

func (c Certificate) createCryptoMbMessaage() (*anypb.Any, error) {
	config := cryptomb.CryptoMbPrivateKeyMethodConfig{
		// Hardcoded to 10ms, it will be configurable in the future.
		PollDelay: durationpb.New(10 * time.Millisecond),
		PrivateKey: &core.DataSource{
			Specifier: &core.DataSource_InlineBytes{
				InlineBytes: c.PrivateKey,
			},
		},
	}
	return messageToAny(&config)
}

func (c Certificate) createTLSContext() (*auth.DownstreamTlsContext, error) {
	tlsCertificates, err := c.createTLScertificates()
	if err != nil {
		return nil, err
	}

	return &auth.DownstreamTlsContext{
		CommonTlsContext: &auth.CommonTlsContext{
			AlpnProtocols: []string{"h2", "http/1.1"},
			// Temporary fix until we start using envoyproxy image newer than v1.23.0 (envoyproxy has adopted TLS v1.2 as the default minimum version in https://github.com/envoyproxy/envoy/commit/f8baa480ec9c6cbaa7a9d5433102efb04145cfc8)
			TlsParams: &auth.TlsParameters{
				TlsMinimumProtocolVersion: auth.TlsParameters_TLSv1_2,
				CipherSuites:              c.CipherSuites,
			},
			TlsCertificates: []*auth.TlsCertificate{tlsCertificates},
		},
	}, nil
}

func (c Certificate) createTLScertificates() (*auth.TlsCertificate, error) {
	switch c.PrivateKeyProvider {
	case "":
		return &auth.TlsCertificate{
			CertificateChain: &core.DataSource{
				Specifier: &core.DataSource_InlineBytes{InlineBytes: c.Certificate},
			},
			PrivateKey: &core.DataSource{
				Specifier: &core.DataSource_InlineBytes{InlineBytes: c.PrivateKey},
			}}, nil
	case "cryptomb":
		msg, err := c.createCryptoMbMessaage()
		if err != nil {
			return nil, err
		}
		return &auth.TlsCertificate{
			CertificateChain: &core.DataSource{
				Specifier: &core.DataSource_InlineBytes{InlineBytes: c.Certificate},
			},
			PrivateKeyProvider: &auth.PrivateKeyProvider{
				ProviderName: "cryptomb",
				ConfigType: &auth.PrivateKeyProvider_TypedConfig{
					TypedConfig: msg,
				},
			}}, nil
	default:
		return nil, errors.New("Unsupported private key provider: " + c.PrivateKeyProvider)
	}
}

// Ref: https://www.envoyproxy.io/docs/envoy/latest/configuration/listeners/listener_filters/proxy_protocol
func createProxyProtocolListenerFilter() (listenerFilter *listener.ListenerFilter, err error) {
	listenerFilterConfig, err := anypb.New(&prx.ProxyProtocol{})
	if err != nil {
		return nil, err
	}

	return &listener.ListenerFilter{
		Name: wellknown.ProxyProtocol,
		ConfigType: &listener.ListenerFilter_TypedConfig{
			TypedConfig: listenerFilterConfig,
		},
	}, nil
}
