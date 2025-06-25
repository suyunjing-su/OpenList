# CDN构建版本说明

## 概述

现在构建脚本支持同时生成两个版本的程序：

1. **标准版本**：包含所有前端静态文件（与之前行为一致）
2. **CDN版本**：只包含index.html，其他静态文件从CDN获取

## 构建方式

### 开发版本
```bash
./build.sh dev
```

### 发布版本
```bash
./build.sh release
```

## 输出文件

构建完成后，在`build/`或`dist/`目录中会生成以下文件：

### 标准版本（与之前一致）
- `openlist-linux-amd64`
- `openlist-windows-amd64.exe`
- `openlist-darwin-amd64`
- 等等...

### CDN版本（新增）
- `openlist-linux-amd64-cdn`
- `openlist-windows-amd64-cdn.exe`
- `openlist-darwin-amd64-cdn`
- 等等...

## CDN配置

CDN版本会从以下URL获取静态资源：
```
https://registry.npmmirror.com/@openlist-frontend/openlist-frontend/$version/files/dist
```

其中`$version`会被自动替换为实际的前端版本号。

## 使用方式

### 标准版本
直接运行，所有静态文件都内嵌在程序中：
```bash
./openlist-linux-amd64
```

### CDN版本
需要设置CDN环境变量：
```bash
export CDN="https://registry.npmmirror.com/@openlist-frontend/openlist-frontend/$version/files/dist"
./openlist-linux-amd64-cdn
```

或者在配置文件中设置CDN字段。

## 优势

### 标准版本
- 无需外部依赖
- 可离线运行
- 部署简单

### CDN版本
- 程序体积更小
- 静态资源加载更快（CDN加速）
- 减少服务器带宽消耗
- 前端更新无需重新部署后端

## 技术实现

1. **构建标签**：使用Go的构建标签区分两个版本
   - 标准版本：不使用特殊标签
   - CDN版本：使用`-tags="cdn"`

2. **静态文件处理**：
   - 标准版本：嵌入完整的`public/dist`目录
   - CDN版本：只嵌入`public/dist-cdn`目录（仅包含index.html等必要文件）

3. **运行时检测**：程序运行时会检测构建标签，决定是否从CDN加载静态资源

## 注意事项

1. CDN版本需要确保CDN地址可访问
2. 如果CDN不可用，静态资源将无法加载
3. 建议在生产环境中使用可靠的CDN服务