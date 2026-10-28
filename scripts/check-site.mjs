#!/usr/bin/env node
// 介绍站的静态自检：本地文件引用是否存在、锚点是否有着落、图片是否有 alt。
// 没有构建步骤，所以这些错误只能靠这里拦住。
import { readFileSync, existsSync } from "node:fs";
import { dirname, join, resolve, relative } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const siteDir = resolve(process.argv[2] || join(here, "..", ".github", "site"));

const pages = ["index.html", "404.html"];
const errors = [];
const ids = new Set();

const read = (page) => {
  const file = join(siteDir, page);
  if (!existsSync(file)) {
    errors.push(`${page}: 文件不存在`);
    return "";
  }
  return readFileSync(file, "utf8");
};

const stripRef = (ref) => ref.split("#")[0].split("?")[0];

const checkRef = (page, ref, kind) => {
  if (!ref) return;
  if (/^(https?:|mailto:|data:|tel:)/i.test(ref)) return;
  if (ref.startsWith("#")) return; // 锚点在下面单独检查
  if (ref.startsWith("//")) {
    errors.push(`${page}: ${kind} 使用协议相对地址（Pages 子路径下容易出错）：${ref}`);
    return;
  }
  if (ref.startsWith("/")) {
    errors.push(`${page}: ${kind} 使用绝对路径（Pages 部署在子路径，会 404）：${ref}`);
    return;
  }
  const target = resolve(siteDir, stripRef(ref));
  if (!target.startsWith(siteDir)) {
    errors.push(`${page}: ${kind} 指向站点目录之外：${ref}`);
    return;
  }
  if (!existsSync(target)) {
    errors.push(`${page}: ${kind} 指向不存在的文件：${ref}`);
  }
};

for (const page of pages) {
  const html = read(page);
  if (!html) continue;

  for (const match of html.matchAll(/\bid="([^"]+)"/g)) {
    if (page === "index.html") ids.add(match[1]);
  }

  for (const match of html.matchAll(/\b(?:src|href)="([^"]*)"/g)) {
    checkRef(page, match[1], "链接");
  }

  for (const match of html.matchAll(/<img\b[^>]*>/g)) {
    if (!/\balt=/.test(match[0])) {
      errors.push(`${page}: <img> 缺少 alt：${match[0].slice(0, 80)}`);
    }
  }
}

const index = read("index.html");
for (const match of index.matchAll(/href="#([^"]*)"/g)) {
  const id = match[1];
  if (id && !ids.has(id)) {
    errors.push(`index.html: 锚点 #${id} 在页面里不存在`);
  }
}

// 首页里引用的截图必须全部收进站点，避免源码里更新了截图而介绍站漏拷。
for (const match of index.matchAll(/assets\/images\/([a-z0-9-]+)\.png/g)) {
  if (!existsSync(join(siteDir, "assets", "images", `${match[1]}.png`))) {
    errors.push(`index.html: 缺少截图 assets/images/${match[1]}.png`);
  }
}

if (errors.length) {
  console.error(`介绍站自检失败（${relative(process.cwd(), siteDir)}）：`);
  for (const error of errors) console.error(`  - ${error}`);
  process.exit(1);
}

console.log(`介绍站自检通过：${relative(process.cwd(), siteDir)}（${pages.join(", ")}）`);
