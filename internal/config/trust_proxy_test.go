package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_TrustProxy(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    []string
		wantErr string
	}{
		{name: "unset uses default", yaml: "", want: DefaultTrustProxy},
		{name: "null uses default", yaml: "trustProxy:\n", want: DefaultTrustProxy},
		{name: "cidr list", yaml: "trustProxy:\n  - 203.0.113.0/24\n  - 198.51.100.0/24\n", want: []string{"203.0.113.0/24", "198.51.100.0/24"}},
		{name: "bare ips become host routes", yaml: "trustProxy:\n  - 192.0.2.10\n  - 2001:db8::1\n", want: []string{"192.0.2.10/32", "2001:db8::1/128"}},
		{name: "cidr is canonicalized", yaml: "trustProxy:\n  - 10.1.2.3/8\n", want: []string{"10.0.0.0/8"}},
		{name: "proxy-addr names", yaml: "trustProxy:\n  - loopback\n", want: []string{"127.0.0.0/8", "::1/128"}},
		{name: "comma separated string", yaml: "trustProxy: '192.0.2.10, 10.0.0.0/8'\n", want: []string{"192.0.2.10/32", "10.0.0.0/8"}},
		{name: "false trusts none", yaml: "trustProxy: false\n", want: []string{}},
		{name: "empty list trusts none", yaml: "trustProxy: []\n", want: []string{}},
		{name: "empty string trusts none", yaml: "trustProxy: ''\n", want: []string{}},
		{name: "true is rejected", yaml: "trustProxy: true\n", wantErr: "trustProxy: true"},
		{name: "hop count is rejected", yaml: "trustProxy: 1\n", wantErr: "hop count"},
		{name: "invalid entry is rejected", yaml: "trustProxy:\n  - 10.0.0.0/8\n  - proxy.internal\n", wantErr: `"proxy.internal"`},
		{name: "invalid cidr is rejected", yaml: "trustProxy:\n  - 10.0.0.0/33\n", wantErr: "10.0.0.0/33"},
		{name: "non-string entry is rejected", yaml: "trustProxy:\n  - 1\n", wantErr: "not a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeTestConfig(t, testYAML+"\n"+tt.yaml))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.TrustProxy)
			require.NotNil(t, cfg.TrustProxy)
		})
	}
}

func TestLoad_TrustProxyFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    []string
		wantErr string
	}{
		{name: "comma separated", env: "10.0.0.0/8,192.0.2.1", want: []string{"10.0.0.0/8", "192.0.2.1/32"}},
		{name: "false", env: "false", want: []string{}},
		{name: "true", env: "TRUE", wantErr: "trustProxy: true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MK_TRUSTPROXY", tt.env)
			cfg, err := Load(writeTestConfig(t, testYAML))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.TrustProxy)
		})
	}
}

func TestResolveTrustProxy_Types(t *testing.T) {
	got, err := resolveTrustProxy([]string{" 10.0.0.0/8 ", ""})
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8"}, got)

	_, err = resolveTrustProxy(map[string]any{"a": 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported type")
}

func TestParseTrustProxy(t *testing.T) {
	t.Run("nil means default", func(t *testing.T) {
		nets, err := ParseTrustProxy(nil)
		require.NoError(t, err)
		assert.Len(t, nets, len(DefaultTrustProxy))
	})
	t.Run("empty means none", func(t *testing.T) {
		nets, err := ParseTrustProxy([]string{})
		require.NoError(t, err)
		assert.Empty(t, nets)
	})
	t.Run("mixed forms", func(t *testing.T) {
		nets, err := ParseTrustProxy([]string{"::1/128", "fc00::/7", "192.0.2.1", "uniquelocal", "LinkLocal"})
		require.NoError(t, err)
		require.Len(t, nets, 9)
		assert.Equal(t, "192.0.2.1/32", nets[2].String())
		assert.Equal(t, "169.254.0.0/16", nets[7].String())
	})
	t.Run("invalid is an error", func(t *testing.T) {
		_, err := ParseTrustProxy([]string{"10.0.0.0/8", "invalid"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"invalid"`)
	})
}
