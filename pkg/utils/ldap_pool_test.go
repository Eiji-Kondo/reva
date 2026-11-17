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
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// These tests need a real LDAPS server. They are skipped unless
// REVA_LDAP_TEST_ADDR is set, e.g.
//
//	REVA_LDAP_TEST_ADDR=127.0.0.1:636 REVA_LDAP_TEST_CACERT=/etc/ldap/tls/ca.crt \
//	REVA_LDAP_TEST_BIND=cn=admin,dc=home,dc=lan REVA_LDAP_TEST_PW=admin \
//	REVA_LDAP_TEST_USERDN=uid=einstein,ou=users,dc=home,dc=lan REVA_LDAP_TEST_USERPW=relativity \
//	go test ./pkg/utils -run LDAPPool -v
func testLDAPConn(t *testing.T) *LDAPConn {
	t.Helper()
	addr := os.Getenv("REVA_LDAP_TEST_ADDR")
	if addr == "" {
		t.Skip("REVA_LDAP_TEST_ADDR not set; skipping LDAP pool integration test")
	}
	host, portStr, err := splitHostPort(addr)
	if err != nil {
		t.Fatalf("bad REVA_LDAP_TEST_ADDR %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port in %q: %v", addr, err)
	}
	return &LDAPConn{
		Hostname:     host,
		Port:         port,
		CACert:       os.Getenv("REVA_LDAP_TEST_CACERT"),
		BindUsername: os.Getenv("REVA_LDAP_TEST_BIND"),
		BindPassword: os.Getenv("REVA_LDAP_TEST_PW"),
	}
}

func splitHostPort(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return "", "", os.ErrInvalid
}

// TestLDAPPoolReusesConnection checks that returning a connection to the pool
// and asking for another hands back the same *ldap.Conn instead of dialing.
func TestLDAPPoolReusesConnection(t *testing.T) {
	c := testLDAPConn(t)

	l1, err := GetLDAPConnection(c)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	PutLDAPConnection(c, l1)

	l2, err := GetLDAPConnection(c)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	defer PutLDAPConnection(c, l2)

	if l1 != l2 {
		t.Fatal("pool did not reuse the returned connection")
	}
}

// TestLDAPPoolRestoresAdminBind checks that after a connection is bound as a
// user, returning it restores the admin bind so a subsequent search works.
func TestLDAPPoolRestoresAdminBind(t *testing.T) {
	c := testLDAPConn(t)
	userDN := os.Getenv("REVA_LDAP_TEST_USERDN")
	userPW := os.Getenv("REVA_LDAP_TEST_USERPW")
	if userDN == "" || userPW == "" {
		t.Skip("REVA_LDAP_TEST_USERDN/PW not set")
	}

	l, err := GetLDAPConnection(c)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := BindLDAPUser(l, userDN, userPW); err != nil {
		t.Fatalf("user bind: %v", err)
	}
	PutLDAPConnection(c, l)

	l2, err := GetLDAPConnection(c)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	defer PutLDAPConnection(c, l2)

	// A search as the admin must succeed; if the user bind had leaked, this
	// would fail with insufficient access.
	req := ldap.NewSearchRequest(
		"dc=home,dc=lan",
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=*)", []string{"dn"}, nil,
	)
	if _, err := l2.Search(req); err != nil {
		t.Fatalf("search after pool round-trip: %v", err)
	}
}

// TestLDAPPoolAvoidsRehandshake is a coarse timing check: pooled re-use must be
// much cheaper than a fresh TLS handshake per call.
func TestLDAPPoolAvoidsRehandshake(t *testing.T) {
	c := testLDAPConn(t)
	const n = 20

	start := time.Now()
	for i := 0; i < n; i++ {
		l, err := dialLDAP(c)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_ = l.Close()
	}
	fresh := time.Since(start)

	start = time.Now()
	for i := 0; i < n; i++ {
		l, err := GetLDAPConnection(c)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		PutLDAPConnection(c, l)
	}
	pooled := time.Since(start)

	t.Logf("n=%d fresh=%v pooled=%v", n, fresh, pooled)
	if pooled >= fresh {
		t.Fatalf("pooled (%v) not faster than fresh dials (%v)", pooled, fresh)
	}
}
