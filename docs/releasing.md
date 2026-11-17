# 发布

npm 包名：顶层 `@youmkdev/ds-tools`，平台包 `ds-tools-<os>-<arch>`（`ds-tools` 与已有 `dstools` 过于相似，registry 拒绝）。

推 tag 即完成发布：`.github/workflows/release.yml` 构建 5 平台产物、创建 GitHub Release，并通过 npm Trusted Publishing（OIDC）发布 5 个包，无需 token、无需人工批准。

首次配置：5 个包各自 Settings → Trusted publishing → GitHub Actions（`Ming3z` / `ds-tools` / `release.yml`，勾选允许 `npm publish`）。配置后 2 天内需成功发布一次完成绑定。

npm 版本不可覆盖：发布失败需修好后升版本号重发；`dist/npm/` 与 `dist/release/` 均不入库。
