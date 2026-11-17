// Copyright 2018-2024 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package utils

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/pkg/errors"
)

// LDAPConn holds the basic parameter for setting up an
// LDAP connection.
type LDAPConn struct {
	Hostname     string `mapstructure:"hostname"`
	Port         int    `mapstructure:"port"`
	Insecure     bool   `docs:"false;Whether to skip certificate checks when sending requests." mapstructure:"insecure"`
	CACert       string `mapstructure:"cacert"`
	BindUsername string `mapstructure:"bind_username"`
	BindPassword string `mapstructure:"bind_password"`
	// MaxIdle is the number of idle connections kept per pool key. Zero keeps
	// the default (8).
	MaxIdle int `mapstructure:"max_idle"`
	// IdleTimeout is how long an idle connection is kept before it is closed.
	// Zero keeps the default (60s).
	IdleTimeout time.Duration `mapstructure:"idle_timeout"`
}

const (
	defaultMaxIdle     = 8
	defaultIdleTimeout = 60 * time.Second
)

// poolKey groups connections that share the same endpoint and bind identity, so
// a pooled connection is only ever handed to a caller with the same config.
func (c *LDAPConn) poolKey() string {
	return fmt.Sprintf("%s:%d|%s", c.Hostname, c.Port, c.BindUsername)
}

func (c *LDAPConn) maxIdle() int {
	if c.MaxIdle > 0 {
		return c.MaxIdle
	}
	return defaultMaxIdle
}

func (c *LDAPConn) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return defaultIdleTimeout
}

type pooledConn struct {
	conn      *ldap.Conn
	idleSince time.Time
}

type connPool struct {
	mu    sync.Mutex
	idle  map[string][]pooledConn
	admin map[*ldap.Conn]bool
}

var ldapPool = &connPool{
	idle:  map[string][]pooledConn{},
	admin: map[*ldap.Conn]bool{},
}

// GetLDAPConnection returns an LDAPS connection, reusing an idle pooled
// connection when one is available and still alive, otherwise dialing a fresh
// one. Callers must return the connection with PutLDAPConnection instead of
// closing it, so it can be reused.
func GetLDAPConnection(c *LDAPConn) (*ldap.Conn, error) {
	key := c.poolKey()
	now := time.Now()

	ldapPool.mu.Lock()
	idle := ldapPool.idle[key]
	for len(idle) > 0 {
		entry := idle[len(idle)-1]
		idle = idle[:len(idle)-1]
		if entry.conn.IsClosing() || now.Sub(entry.idleSince) > c.idleTimeout() {
			_ = entry.conn.Close()
			delete(ldapPool.admin, entry.conn)
			continue
		}
		ldapPool.idle[key] = idle
		ldapPool.mu.Unlock()
		return entry.conn, nil
	}
	ldapPool.idle[key] = idle
	ldapPool.mu.Unlock()

	l, err := dialLDAP(c)
	if err != nil {
		return nil, err
	}
	ldapPool.mu.Lock()
	if c.BindUsername != "" && c.BindPassword != "" {
		ldapPool.admin[l] = true
	}
	ldapPool.mu.Unlock()
	return l, nil
}

// PutLDAPConnection returns a connection to the pool. A connection that was
// bound as a user (see BindLDAPUser) is re-bound as the admin first, so the next
// caller gets an admin-bound connection; a connection that fails to rebind is
// closed instead of pooled.
func PutLDAPConnection(c *LDAPConn, l *ldap.Conn) {
	if l == nil {
		return
	}
	key := c.poolKey()
	now := time.Now()

	ldapPool.mu.Lock()
	wasAdmin := ldapPool.admin[l]
	if !wasAdmin {
		ldapPool.mu.Unlock()
		// Bound as a user (or unknown): restore the admin bind before reuse.
		if c.BindUsername != "" && c.BindPassword != "" {
			if err := l.Bind(c.BindUsername, c.BindPassword); err != nil {
				_ = l.Close()
				return
			}
		}
		ldapPool.mu.Lock()
	}

	idle := ldapPool.idle[key]
	if len(idle) >= c.maxIdle() {
		ldapPool.mu.Unlock()
		delete(ldapPool.admin, l)
		_ = l.Close()
		return
	}
	ldapPool.idle[key] = append(idle, pooledConn{conn: l, idleSince: now})
	ldapPool.admin[l] = true
	ldapPool.mu.Unlock()
}

// BindLDAPUser binds a pooled connection as an end user, e.g. to verify a
// password. The connection stays in the pool; PutLDAPConnection restores the
// admin bind when it is returned.
func BindLDAPUser(l *ldap.Conn, username, password string) error {
	if l == nil {
		return errors.New("ldap: cannot bind a nil connection")
	}
	if err := l.Bind(username, password); err != nil {
		return err
	}
	ldapPool.mu.Lock()
	delete(ldapPool.admin, l)
	ldapPool.mu.Unlock()
	return nil
}

// dialLDAP opens a new LDAPS connection and binds as the admin, if configured.
func dialLDAP(c *LDAPConn) (*ldap.Conn, error) {
	tlsconfig := &tls.Config{InsecureSkipVerify: c.Insecure}

	if !c.Insecure && c.CACert != "" {
		if pemBytes, err := os.ReadFile(c.CACert); err == nil {
			rpool, _ := x509.SystemCertPool()
			rpool.AppendCertsFromPEM(pemBytes)
			tlsconfig.RootCAs = rpool
		} else {
			return nil, errors.Wrapf(err, "Error reading LDAP CA Cert '%s.'", c.CACert)
		}
	}
	l, err := ldap.DialTLS("tcp", fmt.Sprintf("%s:%d", c.Hostname, c.Port), tlsconfig)
	if err != nil {
		return nil, err
	}

	if c.BindUsername != "" && c.BindPassword != "" {
		err = l.Bind(c.BindUsername, c.BindPassword)
		if err != nil {
			l.Close()
			return nil, err
		}
	}
	return l, nil
}
