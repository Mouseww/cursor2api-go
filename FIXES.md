# Cursor2API-Go 修复说明

## 修复的问题

### 1. 模型名称格式错误
**问题：** 原代码使用简单的模型名（如 `claude-4.5-sonnet`），但 Cursor API 实际使用 `provider/model-name` 格式。

**修复：**
- 更新 `models/model_config.go`，添加正确格式的模型配置
- 添加 `convertModelName()` 函数自动转换模型名称
- 保留旧格式作为别名，自动映射到新格式

**已验证可用的模型：**
- `anthropic/claude-sonnet-4.6` - 稳定，指令遵循好
- `anthropic/claude-opus-4.6` - 推理能力强
- `google/gemini-3.1-pro` - 初步测试良好

### 2. SCRIPT_URL 无法访问
**问题：** `https://cursor.com/_next/static/chunks/pages/_app.js` 无法访问，导致程序报错。

**修复：**
- 修改 `services/cursor.go` 中的 `fetchXIsHuman()` 函数
- 添加降级方案：当 SCRIPT_URL 无法访问时，使用空字符串
- 更新默认配置，将 SCRIPT_URL 设为空（启用降级模式）

### 3. Token 限制
**说明：** MaxTokens 设置为 8192，虽然 Cursor 平台实际输出限制约 1200 tokens，但这个参数不影响实际 API 调用，仅用于客户端参考。

## 使用方法

### 环境变量配置

```bash
# .env 文件
PORT=8002
API_KEY=your_api_key_here
MODELS=anthropic/claude-sonnet-4.6,anthropic/claude-opus-4.6,google/gemini-3.1-pro
SCRIPT_URL=  # 留空使用降级方案
TIMEOUT=30
MAX_INPUT_LENGTH=200000
```

### 兼容性

项目现在支持两种模型名称格式：

**新格式（推荐）：**
```json
{
  "model": "anthropic/claude-sonnet-4.6"
}
```

**旧格式（自动转换）：**
```json
{
  "model": "claude-4.5-sonnet"
}
```

旧格式会自动映射到对应的新格式模型。

## 测试

```bash
# 编译
cd /root/.openclaw/agents/tech-dept/projects/cursor2api-go
go build

# 运行
./cursor2api-go

# 测试请求
curl -X POST http://localhost:8002/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your_api_key" \
  -d '{
    "model": "anthropic/claude-sonnet-4.6",
    "messages": [
      {"role": "user", "content": "Hello!"}
    ],
    "stream": false
  }'
```

## 模型选择建议

- **简单指令遵循** → `anthropic/claude-sonnet-4.6`（更快、更便宜）
- **复杂推理任务** → `anthropic/claude-opus-4.6`（推理能力更强）
- **实验性** → `google/gemini-3.1-pro`（初步测试良好）

## 注意事项

1. **实际输出限制**：Cursor 平台实际输出约 1200 tokens，但 MaxTokens 参数不影响 API 调用
2. **内容限制**：拒绝生成长篇创意写作
3. **降级模式**：SCRIPT_URL 留空时使用降级方案，可能影响反爬虫能力

## 修复文件清单

- `models/model_config.go` - 更新模型配置
- `services/cursor.go` - 添加模型名称转换和降级方案
- `config/config.go` - 更新默认配置
- `FIXES.md` - 本文档
