// Copyright 2018-2019 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package server

import (
	"crypto/rsa"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uapolicy"
	"github.com/gopcua/opcua/uasc"
)

// Option is an option function type to modify the configuration.
type Option func(*serverConfig)

// PrivateKey sets the RSA private key in the secure channel configuration.
func PrivateKey(key *rsa.PrivateKey) Option {
	return func(s *serverConfig) {
		s.privateKey = key
	}
}

// EndPointHostName adds an additional endpoint to the server based on the host name
func EndPoint(host string, port int) Option {
	return func(s *serverConfig) {
		if s.endpoints == nil {
			s.endpoints = make([]string, 0)
		}
		ep := fmt.Sprintf("opc.tcp://%s:%d", host, port)
		s.endpoints = append(s.endpoints, ep)
	}
}

// Certificate sets the client X509 certificate in the secure channel configuration
// and also detects and sets the ApplicationURI from the URI within the certificate
func Certificate(cert []byte) Option {
	return func(s *serverConfig) {
		s.certificate = cert

		// Extract the application URI from the certificate.
		var appURI string
		x509cert, err := uapolicy.ParseCertificate(cert)
		if err == nil && len(x509cert.URIs) > 0 {
			appURI = x509cert.URIs[0].String()
		}

		s.applicationURI = appURI
	}
}

// EnableSecurity registers a new endpoint security mode to the server.
// This will also register the security policy against each enabled auth mode
func EnableSecurity(secPolicy string, secMode ua.MessageSecurityMode) Option {
	return func(s *serverConfig) {
		if !strings.HasPrefix(secPolicy, "http://opcfoundation.org/UA/SecurityPolicy#") {
			secPolicy = "http://opcfoundation.org/UA/SecurityPolicy#" + secPolicy
		}

		var ok bool
		ss := uapolicy.SupportedPolicies()
		for _, sp := range ss {
			if sp == secPolicy {
				ok = true
				break
			}
		}
		if !ok {
			log.Printf("error adding endpoint to config, %s is unsupported", secPolicy)
			return
		}

		for _, sec := range s.enabledSec {
			if sec.secPolicy == secPolicy && sec.secMode == secMode {
				if s.logger != nil {
					s.logger.Warn("security policy already exists, skipping")
				}
				return
			}
		}

		sec := security{
			secPolicy: secPolicy,
			secMode:   secMode,
		}

		s.enabledSec = append(s.enabledSec, sec)
	}
}

// EnableAuthMode registers a new user authentication mode to the server.
// All AuthModes except Anonymous require encryption by default, so EnableSecurity()
// must also be called with at least one non-"None" SecurityPolicy
func EnableAuthMode(tokenType ua.UserTokenType) Option {
	return func(s *serverConfig) {

		for _, a := range s.enabledAuth {
			if a.tokenType == tokenType {
				if s.logger != nil {
					s.logger.Warn("auth mode already registered, skipping")
				}
				return
			}
		}

		a := authMode{
			tokenType: tokenType,
		}

		s.enabledAuth = append(s.enabledAuth, a)
	}
}

func defaultChannelConfig() *uasc.Config {
	return &uasc.Config{
		SecurityPolicyURI: ua.SecurityPolicyURINone,
		SecurityMode:      ua.MessageSecurityModeNone,
		Lifetime:          uint32(time.Hour / time.Millisecond),
	}
}

// MaxSubscriptions sets the largest number of Subscriptions the server holds
// at a time, for all Sessions together. While it is reached, CreateSubscription
// answers Bad_TooManySubscriptions (Part 4 §5.14.2.4, Table 83). The server
// reports it as ServerCapabilities.MaxSubscriptions (Part 5 §6.3.2), and 0
// when no limit is set. A value of zero or less means no limit, the default;
// a value above math.MaxUint32 is taken as math.MaxUint32.
func MaxSubscriptions(n int) Option {
	return func(s *serverConfig) {
		s.cap.MaxSubscriptions = subscriptionLimit(n)
	}
}

// MaxSubscriptionsPerSession sets the largest number of Subscriptions one
// Session holds at a time. While it is reached, CreateSubscription in that
// Session answers Bad_TooManySubscriptions (Part 4 §5.14.2.4, Table 83). The
// server reports it as ServerCapabilities.MaxSubscriptionsPerSession
// (Part 5 §6.3.2), and 0 when no limit is set. A value of zero or less means
// no limit, the default; a value above math.MaxUint32 is taken as
// math.MaxUint32.
func MaxSubscriptionsPerSession(n int) Option {
	return func(s *serverConfig) {
		s.cap.MaxSubscriptionsPerSession = subscriptionLimit(n)
	}
}

// subscriptionLimit converts a limit option to its UInt32 capability value.
func subscriptionLimit(n int) uint32 {
	if n <= 0 {
		return 0
	}
	return uint32(min(uint64(n), math.MaxUint32))
}

func ServerName(name string) Option {
	return func(s *serverConfig) {
		s.applicationName = name
	}
}

func ManufacturerName(name string) Option {
	return func(s *serverConfig) {
		s.manufacturerName = name
	}
}

func ProductName(name string) Option {
	return func(s *serverConfig) {
		s.productName = name
	}
}

func SoftwareVersion(name string) Option {
	return func(s *serverConfig) {
		s.softwareVersion = name
	}
}

// this logger interface is used to allow the user to provide their own logger
// it is compatible with slog.Logger
type Logger interface {
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// the server.SetLogger takes a server.Logger interface.  This interface is met by
// slog.Logger{}.  A simple wrapper could be made for other loggers if they don't already
// meet the interface.
func SetLogger(logger Logger) Option {
	return func(s *serverConfig) {
		s.logger = logger
	}
}
