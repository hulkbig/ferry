# 参与 Ferry

[English](CONTRIBUTING.en.md)

一次可复现的工具续轮问题、一份准确的渠道能力证据，或一段更清楚的安装说明，都是有用的贡献。欢迎使用中文或英文交流。

## 先选对入口

- 使用和安装问题：先读[常见问题](docs/guide/faq.md)和[排障指南](docs/guide/troubleshooting.md)。
- 可复现的缺陷：使用 [Bug report](https://github.com/hulkbig/ferry/issues/new?template=bug_report.yml)。
- 新功能或较大的设计变更：先用 [Feature request](https://github.com/hulkbig/ferry/issues/new?template=feature_request.yml)说明场景和现有替代办法，避免做完后才发现范围不合适。
- 小型修复、翻译和文档改进：可以直接提 PR，无需先开 Issue。
- 安全问题：按[安全报告说明](SECURITY.md)私密报告，不提交公开复现。

对外协作入口是 `hulkbig/ferry`。这些 GitHub 入口在首次公开发行后可用。

## 从修改到 PR

1. Fork 公开仓库，克隆自己的 fork，从仓库默认分支新建任务分支。首次发行默认分支为 `main`，后续以 GitHub 显示为准。
2. 按[开发指南](docs/development.md)准备环境。一次改动围绕一个问题；避免混入无关格式化、生成文件或依赖升级。
3. 修改行为时补充能复现问题的测试，并同步相关文档。翻译保留同样的限制和示例，不只翻译标题。
4. 执行与改动有关的检查。在 PR 中列出命令、结果和未执行项；区分模拟上游、离线测试与真实账号验证。
5. 阅读下方 DCO 说明，以自己的公开身份签署提交，推送到自己的 fork。
6. 向 `hulkbig/ferry` 的默认分支发起 PR。尚未完成时可用 Draft；准备好审阅后再标记 Ready for review。

PR 标题描述解决的问题。正文说明触发条件、改动前后的行为、验证结果及兼容影响；有 Issue 时关联它。UI 变化附脱敏前后截图，标明语言、主题和屏幕宽度。不要上传真实账号、数据库、备份或用户正文。

维护者可能要求最小复现、调整范围或补充测试。按审阅意见更新同一个 PR；CI 通过不等于已合并，也不代表真实上游已验收。项目不承诺固定审阅时间。

## 选择合适的检查

```sh
# 示例：管理接口改动
go test ./internal/management

# 控制台行为
npm test --prefix web/console

# 完整产品检查
bash scripts/test.sh
```

文档修正核对命令、链接和两种语言即可。协议、权限、数据迁移和并发变化需要覆盖对应失败路径；并发代码另做相关 race 检查。完整环境和命令见[开发指南](docs/development.md)。

## 新增渠道与兼容规则

先给出官方协议或客户端的准确版本与来源，再附脱敏复现。第三方项目可以提供兼容线索，但不能代替独立验证。说明认证、协议转换和厂商适配分别改了哪里，以及哪些字段会保留、降级或拒绝。提供流式终态、工具调用 ID、取消和错误路径的相关回归用例。见[渠道适配边界](docs/provider-adapters.md)。

## 来源与署名

原创贡献采用 Apache-2.0。复制或改写第三方代码时注明项目、准确版本、许可证和修改范围，并保留原有声明；字体、图标和其他资产同样需要来源。

贡献使用 [Developer Certificate of Origin 1.1](https://developercertificate.org/)。阅读并确认能作出其中承诺后，使用：

```sh
git commit -s
```

`Signed-off-by` 使用你愿意公开的姓名与邮箱，不代他人签署。AI 辅助生成的修改也应由提交者理解、验证并核对来源。第三方权利见 [NOTICE](NOTICE) 和 [LICENSES](LICENSES)。

## 讨论方式

围绕代码、问题和证据讨论，尊重不同经验与意见。不骚扰、不进行人身攻击，也不发布他人私人信息。维护者可关闭重复、无法验证或超出维护范围的请求，并说明原因。
