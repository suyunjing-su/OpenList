# Build.sh 参数支持说明

## 新增的第三个参数支持

现在build.sh支持第三个参数来控制构建模式：

### 参数说明

1. **第一个参数**：版本类型
   - `dev` - 开发版本
   - `beta` - 测试版本  
   - `release` - 发布版本

2. **第二个参数**：构建类型
   - `docker` - Docker构建
   - `docker-multiplatform` - Docker多平台构建
   - `linux_musl` - Linux musl构建
   - `linux_musl_arm` - Linux musl ARM构建
   - `android` - Android构建
   - `freebsd` - FreeBSD构建
   - `web` - 仅Web（不构建）

3. **第三个参数**：构建模式（新增）
   - `cdn-only` - 只构建CDN版本
   - `standard-only` - 只构建标准版本
   - 默认（空）- 构建两个版本

## 使用示例

```bash
# 构建两个版本（默认行为）
bash build.sh dev
bash build.sh beta docker-multiplatform
bash build.sh release

# 只构建CDN版本
bash build.sh dev "" cdn-only
bash build.sh beta docker-multiplatform cdn-only
bash build.sh release docker cdn-only

# 只构建标准版本
bash build.sh dev "" standard-only
bash build.sh beta docker-multiplatform standard-only
bash build.sh release docker standard-only
```

## 实现的功能

### CDN版本特点
- 程序体积更小
- 只包含index.html等必要文件
- 静态资源从CDN获取
- 文件名带有`-cdn`后缀

### 标准版本特点
- 包含所有前端文件
- 可离线运行
- 无外部依赖
- 保持原有文件名

## 技术实现

1. **参数传递**：所有构建函数现在接收三个参数
2. **条件构建**：根据第三个参数决定构建哪个版本
3. **文件管理**：CDN版本使用`public/dist-cdn`目录
4. **构建标签**：CDN版本使用`-tags="jsoniter cdn"`

## 注意事项

- 如果第二个参数为空但需要指定第三个参数，请使用空字符串`""`
- CDN版本需要确保CDN地址可访问
- 构建时会自动创建必要的目录结构