
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
    "maxRequests": 1.2
  }
}


```

**golang struct**:
```
type AppSettings struct {
	Port        string  
	MaxRequests float64 
}

// Settings contains the config.yml settings
type Settings struct {
	// App
	Name string      `mapstructure:"name"` 
	App  AppSettings `mapstructure:"app"`
}

```

**Note:** Avoid using underscores and stick to using camelCase. Instead of `max_requests` use `maxRequests`.

