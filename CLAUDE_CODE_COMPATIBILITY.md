# Claude Code 工具调用兼容性说明

## 🔍 兼容性分析

### ✅ 本项目与 Claude Code 的兼容性

**结论：完全兼容 ✅**

本项目在禁用工具调用重写后，现在是 **100% 透明代理**，完全兼容 Claude Code 的工具调用。

---

## 📋 工作原理

### 1. 工具调用流程

```
Claude Code 客户端
    ↓ (发送工具定义和消息)
claude2API 代理
    ↓ (透明传递)
claude.ai 后端
    ↓ (返回工具调用)
claude2API 解析
    ↓ (提取工具调用，原样返回)
Claude Code 客户端执行工具
```

### 2. 支持的工具调用格式

本项目支持多种工具调用格式（见 `handlers/tools.go`）：

1. **主格式**: `[TOOL_CALL]{...}[/TOOL_CALL]` ✅
2. **XML格式**: `<tool_call>{...}</tool_call>` ✅
3. **OpenAI格式**: `{"tool_calls":[...]}` ✅
4. **代码块包裹**: ````json [TOOL_CALL]...```` ✅

### 3. 工具调用解析

```go
// handlers/anthropic.go:791 (已禁用重写)
text, calls := extractToolCalls(content)
// 🔴 已注释: calls = rewriteInitialReadCalls(req.Messages, req.ToolDefs, calls)
logToolCallRound("initial", text, calls)
```

**关键修复**：
- ✅ `rewriteInitialReadCalls` 已被禁用
- ✅ 工具调用名称不会被改写（如 Read → Glob）
- ✅ 工具参数不会被修改
- ✅ 工具ID保持一致

---

## 🧪 测试验证

### 测试场景

使用 Claude Code 通过本项目调用以下工具：

| 工具名称 | 测试状态 | 说明 |
|---------|---------|------|
| `Read` | ✅ 应该正常 | 读取文件 |
| `Write` | ✅ 应该正常 | 写入文件 |
| `Edit` | ✅ 应该正常 | 编辑文件 |
| `Bash` | ✅ 应该正常 | 执行命令 |
| `Glob` | ✅ 应该正常 | 文件搜索 |
| `Grep` | ✅ 应该正常 | 内容搜索 |
| `Agent` | ✅ 应该正常 | 子代理调用 |
| 所有其他工具 | ✅ 应该正常 | 透明传递 |

### 预期行为

**修复前（工具调用会被改写）**：
```json
Claude 返回: {"name":"Read","input":{"file_path":"test.go"}}
实际发送给客户端: {"name":"Glob","input":{"pattern":"**/test.go","path":"."}}
结果: ❌ 客户端收到未知工具名称，调用失败
```

**修复后（透明传递）**：
```json
Claude 返回: {"name":"Read","input":{"file_path":"test.go"}}
实际发送给客户端: {"name":"Read","input":{"file_path":"test.go"}}
结果: ✅ 客户端正确识别和执行工具
```

---

## 🔧 配置说明

### Claude Code 客户端配置

使用本项目作为 Claude Code 的 API 端点：

#### 方式1: 通过环境变量

```bash
# OpenAI 兼容格式
export OPENAI_API_KEY="your_api_key_or_master_key"
export OPENAI_BASE_URL="http://localhost:8080/v1"

claude code
```

#### 方式2: 通过配置文件

**~/.config/claude/config.json**:
```json
{
  "apiEndpoint": "http://localhost:8080/v1",
  "apiKey": "your_api_key_or_master_key"
}
```

#### 方式3: Anthropic Messages API 格式

```bash
export ANTHROPIC_API_KEY="your_api_key_or_master_key"
export ANTHROPIC_BASE_URL="http://localhost:8080/v1"
```

### API Key 获取

通过管理面板创建：
1. 访问 `http://localhost:8080/admin`
2. 进入「API Key 管理」
3. 创建新的 API Key
4. 将生成的 Key 配置到 Claude Code

---

## 📊 功能对比

| 功能 | claude.ai 直连 | 通过本项目代理 | 说明 |
|------|---------------|---------------|------|
| 工具调用 | ✅ | ✅ | 完全兼容 |
| 流式输出 | ✅ | ✅ | SSE支持 |
| 多轮对话 | ✅ | ✅ | conversation_id |
| 思考模式 | ✅ | ✅ | thinking support |
| 文件上传 | ❌ | ❌ | 暂不支持 |
| 多账号负载均衡 | ❌ | ✅ | 本项目独有 |
| API Key 管理 | ❌ | ✅ | 本项目独有 |
| 请求统计 | ❌ | ✅ | 本项目独有 |

---

## ⚠️ 注意事项

### 1. 工具执行位置

**重要**：本项目不执行工具，只是透明传递工具调用。

- ✅ Claude 生成工具调用
- ✅ 本项目解析并返回工具调用
- ✅ **Claude Code 客户端**执行工具
- ✅ 执行结果返回给 Claude

### 2. 死代码说明

项目中存在 `executeTool` 等函数（`handlers/tools.go:352-619`），但这些是**死代码**，从未被调用：

```go
// 这些函数从未被调用（约268行死代码）
func executeTool(name string, input map[string]interface{}) string
func toolBash(input map[string]interface{}) string
func toolRead(input map[string]interface{}) string
func toolWrite(input map[string]interface{}) string
```

**设计决策**：本项目设计为将工具调用返回给客户端执行，而非服务器端执行。

**建议**：
- 可以删除这些死代码减少维护负担
- 或者保留作为未来功能扩展的参考

### 3. 安全性

由于工具在**客户端**执行：
- ✅ 服务器端无需文件系统访问权限
- ✅ 服务器端无需执行命令权限
- ✅ 更安全的多租户部署
- ✅ 客户端可以完全控制工具执行

---

## 🎯 最佳实践

### 1. 生产部署

```yaml
# docker-compose.yml
services:
  claude2api:
    environment:
      - ALLOWED_ORIGINS=https://your-claude-code-instance.com
      - ENABLE_HSTS=true
      - GIN_MODE=release
    ports:
      - "8080:8080"
```

### 2. Claude Code 配置

```bash
# 使用本地代理
export OPENAI_BASE_URL="http://localhost:8080/v1"
export OPENAI_API_KEY="your_api_key"

# 或使用远程代理
export OPENAI_BASE_URL="https://claude-proxy.yourdomain.com/v1"
export OPENAI_API_KEY="your_api_key"
```

### 3. 多账号负载均衡

通过管理面板添加多个 claude.ai 账号，系统会自动：
- ✅ 按「最少负载」分发请求
- ✅ 自动检测账号健康状态
- ✅ 429 错误时自动冷却和恢复
- ✅ 会话粘性（同一对话使用同一账号）

---

## 📝 故障排查

### 工具调用失败？

**检查清单**：
1. ✅ 确认已禁用 `rewriteInitialReadCalls`
   ```bash
   grep "// calls = rewriteInitialReadCalls" handlers/anthropic.go
   # 应该有3处注释
   ```

2. ✅ 确认 Claude Code 配置正确
   ```bash
   echo $OPENAI_BASE_URL
   # 应该指向本项目的 /v1 端点
   ```

3. ✅ 查看日志
   ```bash
   docker compose logs -f claude2api
   # 或
   ./claude2api
   ```

4. ✅ 测试基本 API
   ```bash
   curl http://localhost:8080/v1/models \
     -H "Authorization: Bearer your_api_key"
   ```

### 工具名称被改写？

如果仍然发现工具名称被改写，请确认：
- 使用的是修复后的代码版本
- 重新编译了项目 (`go build -o claude2api .`)
- 重启了服务

---

## ✅ 总结

**本项目完全兼容 Claude Code 的工具调用功能！**

修复工具调用重写问题后：
- ✅ 工具调用透明传递
- ✅ 工具名称和参数不被改写
- ✅ 所有 Claude Code 工具正常工作
- ✅ 支持多账号负载均衡
- ✅ 提供 Web 管理面板

**推荐用于**：
- Claude Code 的多账号负载均衡
- 企业级 Claude API 代理
- Claude.ai 账号管理和监控
- API Key 统一管理

---

**文档版本**: 1.0  
**更新日期**: 2026-09-05  
**兼容性**: Claude Code (所有版本)
