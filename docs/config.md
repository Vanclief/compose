
# Config

Simple configuration

## Why:

Every single program uses configuration in the form of settings or env vars

## How:

**environment.config.json**
```
{
  "name": "hi",
  "app": {
    "port": "3000",
    "max_requests": 1.2
  }
}


```

**golang struct**:
```
type AppSettings struct {
	Port        string  `mapstucture:"port"`
	MaxRequests float64 `mapstructure:"max_requests"`
}

// Settings contains the config.yml settings
type Settings struct {
	// App
	Name string      `mapstructure:"name"` 
	App  AppSettings `mapstructure:"app"`
}

```