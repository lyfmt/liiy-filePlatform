package main

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultConfigPath = "./config.yaml"

type authConfig struct {
	Enabled  bool
	Username string
	Password string
	Secret   string
}

type configFile struct {
	Port          string
	DataDir       string
	MaxUploadMB   int64
	AuthEnabled   bool
	Username      string
	Password      string
	SessionSecret string
}

func loadConfig() (config, error) {
	path := strings.TrimSpace(os.Getenv("CONFIG_PATH"))
	if path == "" {
		path = defaultConfigPath
	}

	fileCfg, err := ensureConfig(path, os.Stdin, os.Stdout, isInteractiveFile(os.Stdin))
	if err != nil {
		return config{}, err
	}

	port := strings.TrimSpace(fileCfg.Port)
	if port == "" {
		port = defaultPort
	}
	if raw := strings.TrimSpace(os.Getenv("PORT")); raw != "" {
		port = raw
	}

	dataDir := strings.TrimSpace(fileCfg.DataDir)
	if dataDir == "" {
		dataDir = defaultDataDir
	}
	if raw := strings.TrimSpace(os.Getenv("DATA_DIR")); raw != "" {
		dataDir = raw
	}

	maxUploadMB := fileCfg.MaxUploadMB
	if maxUploadMB <= 0 {
		maxUploadMB = defaultMaxUpload
	}
	maxUploadFixed := false
	if raw := strings.TrimSpace(os.Getenv("MAX_UPLOAD_MB")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			maxUploadMB = parsed
			maxUploadFixed = true
		}
	}

	return config{
		port:           port,
		dataDir:        dataDir,
		maxUploadMB:    maxUploadMB,
		maxUploadBytes: maxUploadMB * 1024 * 1024,
		configPath:     path,
		maxUploadFixed: maxUploadFixed,
		auth: authConfig{
			Enabled:  fileCfg.AuthEnabled,
			Username: fileCfg.Username,
			Password: fileCfg.Password,
			Secret:   fileCfg.SessionSecret,
		},
	}, nil
}

func ensureConfig(path string, in io.Reader, out io.Writer, interactive bool) (configFile, error) {
	cfg, err := readConfigFile(path)
	switch {
	case err == nil:
		changed, generated := applyConfigDefaults(&cfg)
		if changed {
			if err := writeConfigFile(path, cfg); err != nil {
				return configFile{}, err
			}
			if interactive && generated {
				printConfigSummary(out, path, cfg, false)
			}
		}
		return cfg, nil
	case !errors.Is(err, os.ErrNotExist):
		return configFile{}, err
	}

	if !interactive {
		return configFile{}, fmt.Errorf("%s 不存在，请先在终端交互式运行一次以初始化配置", path)
	}

	cfg, err = promptNewConfig(in, out)
	if err != nil {
		return configFile{}, err
	}

	if _, generated := applyConfigDefaults(&cfg); generated {
		// defaults already injected into cfg, summary will print final values
	}

	if err := writeConfigFile(path, cfg); err != nil {
		return configFile{}, err
	}

	printConfigSummary(out, path, cfg, true)
	return cfg, nil
}

func readConfigFile(path string) (configFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return configFile{}, err
	}

	cfg := configFile{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := parseYAMLScalar(parts[1])

		switch key {
		case "port":
			cfg.Port = value
		case "data_dir":
			cfg.DataDir = value
		case "max_upload_mb":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				cfg.MaxUploadMB = parsed
			}
		case "auth_enabled":
			cfg.AuthEnabled = strings.EqualFold(value, "true")
		case "username":
			cfg.Username = value
		case "password":
			cfg.Password = value
		case "session_secret":
			cfg.SessionSecret = value
		}
	}

	if err := scanner.Err(); err != nil {
		return configFile{}, fmt.Errorf("读取配置失败: %w", err)
	}

	return cfg, nil
}

func writeConfigFile(path string, cfg configFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	content := strings.Join([]string{
		"# liiy-filePlatform runtime config",
		"# 修改 username/password 或 auth_enabled 后，重启服务生效",
		fmt.Sprintf("port: %s", yamlQuote(cfg.Port)),
		fmt.Sprintf("data_dir: %s", yamlQuote(cfg.DataDir)),
		fmt.Sprintf("max_upload_mb: %d", cfg.MaxUploadMB),
		fmt.Sprintf("auth_enabled: %t", cfg.AuthEnabled),
		fmt.Sprintf("username: %s", yamlQuote(cfg.Username)),
		fmt.Sprintf("password: %s", yamlQuote(cfg.Password)),
		fmt.Sprintf("session_secret: %s", yamlQuote(cfg.SessionSecret)),
		"",
	}, "\n")

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("写入临时配置失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("替换配置失败: %w", err)
	}
	return nil
}

func promptNewConfig(in io.Reader, out io.Writer) (configFile, error) {
	reader := bufio.NewReader(in)
	cfg := configFile{
		Port:        defaultPort,
		DataDir:     defaultDataDir,
		MaxUploadMB: defaultMaxUpload,
	}

	fmt.Fprintln(out, "未检测到 config.yaml，开始初始化 liiy-filePlatform。")

	enableAuth, err := promptYesNo(reader, out, "是否开启登录校验? [Y/n]: ", true)
	if err != nil {
		return configFile{}, err
	}
	cfg.AuthEnabled = enableAuth

	customize, err := promptYesNo(reader, out, "是否自定义账号和密码? [y/N]: ", false)
	if err != nil {
		return configFile{}, err
	}

	if customize {
		username, err := promptRequired(reader, out, "请输入用户名: ")
		if err != nil {
			return configFile{}, err
		}
		password, err := promptRequired(reader, out, "请输入密码: ")
		if err != nil {
			return configFile{}, err
		}
		cfg.Username = username
		cfg.Password = password
	}

	return cfg, nil
}

func promptYesNo(reader *bufio.Reader, out io.Writer, prompt string, defaultYes bool) (bool, error) {
	for {
		fmt.Fprint(out, prompt)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}

		answer := strings.ToLower(strings.TrimSpace(line))
		if answer == "" {
			return defaultYes, nil
		}

		switch answer {
		case "y", "yes", "是", "开启", "开":
			return true, nil
		case "n", "no", "否", "关闭", "关":
			return false, nil
		}

		if errors.Is(err, io.EOF) {
			return defaultYes, nil
		}
	}
}

func promptRequired(reader *bufio.Reader, out io.Writer, prompt string) (string, error) {
	for {
		fmt.Fprint(out, prompt)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}

		value := strings.TrimSpace(line)
		if value != "" {
			return value, nil
		}

		if errors.Is(err, io.EOF) {
			return "", io.ErrUnexpectedEOF
		}
	}
}

func applyConfigDefaults(cfg *configFile) (changed bool, generatedCredentials bool) {
	if strings.TrimSpace(cfg.Port) == "" {
		cfg.Port = defaultPort
		changed = true
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		cfg.DataDir = defaultDataDir
		changed = true
	}
	if cfg.MaxUploadMB <= 0 {
		cfg.MaxUploadMB = defaultMaxUpload
		changed = true
	}
	if strings.TrimSpace(cfg.Username) == "" {
		cfg.Username = randomCredential(8)
		changed = true
		generatedCredentials = true
	}
	if strings.TrimSpace(cfg.Password) == "" {
		cfg.Password = randomCredential(8)
		changed = true
		generatedCredentials = true
	}
	if strings.TrimSpace(cfg.SessionSecret) == "" {
		cfg.SessionSecret = randomCredential(32)
		changed = true
	}
	return changed, generatedCredentials
}

func printConfigSummary(out io.Writer, path string, cfg configFile, created bool) {
	if created {
		fmt.Fprintf(out, "已生成配置文件: %s\n", path)
	} else {
		fmt.Fprintf(out, "配置文件已补齐缺失项: %s\n", path)
	}
	fmt.Fprintf(out, "登录校验: %t\n", cfg.AuthEnabled)
	fmt.Fprintf(out, "用户名: %s\n", cfg.Username)
	fmt.Fprintf(out, "密码: %s\n", cfg.Password)
	fmt.Fprintln(out, "可随时手动修改 config.yaml 中的用户名和密码。")
}

func parseYAMLScalar(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	return value
}

func yamlQuote(value string) string {
	return strconv.Quote(value)
}

func randomCredential(length int) string {
	if length <= 0 {
		return ""
	}

	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		fallback := strings.Repeat("A1b2C3d4", (length/8)+1)
		return fallback[:length]
	}

	out := make([]byte, length)
	for i, b := range raw {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

func isInteractiveFile(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
