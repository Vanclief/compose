package configurator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

type testConfig struct {
	Name string `mapstructure:"name"`
}

// newTestConfigurator returns a Configurator for the "test" environment that
// reads its config file from dir. viper keeps its state, including the config
// file it found, in a package global, so it is reset before and after.
func newTestConfigurator(t *testing.T, dir string) *Configurator {
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("ENVIRONMENT", "test")

	cfg, err := New(WithConfigPath(dir))
	require.NoError(t, err)
	return cfg
}

func TestLoadConfigurationReadsFile(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "test.config.json"), []byte(`{"name": "compose"}`), 0o600)
	require.NoError(t, err)

	var config testConfig
	err = newTestConfigurator(t, dir).LoadConfiguration(&config)
	require.NoError(t, err)
	assert.Equal(t, "compose", config.Name)
}

func TestLoadConfigurationMissingFile(t *testing.T) {
	dir := t.TempDir()

	var config testConfig
	err := newTestConfigurator(t, dir).LoadConfiguration(&config)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	assert.Equal(t, "Config file with path "+dir+"/test.config.json not found", ez.ErrorMessage(err))
}

func TestLoadConfigurationMalformedFile(t *testing.T) {
	cases := map[string]string{
		"not JSON":           `{"name": `,
		"JSON but no object": `[1]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test.config.json")
			err := os.WriteFile(path, []byte(content), 0o600)
			require.NoError(t, err)

			var config testConfig
			err = newTestConfigurator(t, dir).LoadConfiguration(&config)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			assert.Equal(t, "Config file "+path+" is not a valid JSON object", ez.ErrorMessage(err))
		})
	}
}

func TestLoadConfigurationUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.config.json")
	err := os.WriteFile(path, []byte(`{"name": "compose"}`), 0o000)
	require.NoError(t, err)
	_, err = os.ReadFile(path)
	if err == nil {
		t.Skip("the file is readable without permissions, as for root or on Windows")
	}

	var config testConfig
	err = newTestConfigurator(t, dir).LoadConfiguration(&config)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	assert.Equal(t, "Config file "+path+" could not be read", ez.ErrorMessage(err))
}
