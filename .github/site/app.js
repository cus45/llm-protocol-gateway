/* Landing site behaviour — no dependencies, degrades to plain links. */
(() => {
  "use strict";

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
  const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  document.documentElement.classList.add("js");

  /* ------------------------------------------------------------------ nav */
  const nav = $("#nav");
  if (nav) {
    const syncNav = () => nav.classList.toggle("is-scrolled", window.scrollY > 8);
    syncNav();
    window.addEventListener("scroll", syncNav, { passive: true });
  }

  /* --------------------------------------------------------------- reveal */
  const revealEls = $$(".reveal");
  if (!("IntersectionObserver" in window) || reduceMotion) {
    revealEls.forEach((el) => el.classList.add("is-visible"));
  } else {
    const io = new IntersectionObserver(
      (entries) => {
        entries.forEach((entry) => {
          if (!entry.isIntersecting) return;
          const el = entry.target;
          const siblings = el.parentElement ? Array.from(el.parentElement.children) : [el];
          const delay = Math.min(Math.max(siblings.indexOf(el), 0) * 55, 220);
          el.style.animationDelay = `${delay}ms`;
          el.classList.add("is-visible");
          window.setTimeout(() => {
            el.style.animationDelay = "";
          }, 1100);
          io.unobserve(el);
        });
      },
      { threshold: 0.12, rootMargin: "0px 0px -6% 0px" }
    );
    revealEls.forEach((el) => io.observe(el));
  }

  /* ---------------------------------------------------------- flow footer */
  const flowLine = $("#flow-line");
  if (flowLine && !reduceMotion) {
    const lines = [
      "POST /v1/chat/completions → 200 · 412 ms · 2,841 tok",
      "POST /v1/messages → 200 · 638 ms · 1,204 tok",
      "POST /v1/responses → 200 · 356 ms · stream",
      "GET /v1/models → 200 · 6 ms · 138 个模型",
      "POST /v1/embeddings → 200 · 84 ms · 1,536 dims",
    ];
    let index = 0;
    window.setInterval(() => {
      if (document.hidden) return;
      index = (index + 1) % lines.length;
      flowLine.classList.add("is-fading");
      window.setTimeout(() => {
        flowLine.textContent = lines[index];
        flowLine.classList.remove("is-fading");
      }, 260);
    }, 3800);
  }

  /* --------------------------------------------------------------- matrix */
  const matrix = $("#matrix");
  const matrixStatus = $("#matrix-status");
  const matrixStatusText = matrixStatus ? $("code", matrixStatus) : null;
  if (matrix && matrixStatusText) {
    const names = {
      chat: "OpenAI Chat",
      responses: "OpenAI Responses",
      claude: "Claude",
      gemini: "Gemini",
    };
    const cells = $$("td", matrix);
    const defaultText = matrixStatusText.textContent;

    const describe = (cell) => {
      const from = names[cell.dataset.in] || cell.dataset.in;
      const to = names[cell.dataset.out] || cell.dataset.out;
      const same = cell.dataset.in === cell.dataset.out;
      return same
        ? `${from} → ${to} · 原样透传，只做鉴权与路由，不重写请求体。`
        : `${from} → ${to} · 点对点直转，工具调用、图像、thinking 与缓存断点逐项保留。`;
    };

    const reset = () => {
      $$(".is-col, .is-row, .is-active", matrix).forEach((el) =>
        el.classList.remove("is-col", "is-row", "is-active")
      );
      matrixStatusText.textContent = defaultText;
      matrixStatus.classList.remove("is-live");
    };

    const highlight = (cell) => {
      reset();
      const out = cell.dataset.out;
      const row = cell.closest("tr");
      cells.forEach((c) => {
        if (c.dataset.out === out) c.classList.add("is-col");
      });
      if (row) row.classList.add("is-row");
      cell.classList.add("is-active");
      matrixStatusText.textContent = describe(cell);
      matrixStatus.classList.add("is-live");
    };

    cells.forEach((cell) => {
      cell.tabIndex = 0;
      cell.setAttribute("aria-label", describe(cell));
    });

    matrix.addEventListener("mouseover", (event) => {
      const cell = event.target.closest("td");
      if (cell) highlight(cell);
    });
    matrix.addEventListener("focusin", (event) => {
      const cell = event.target.closest("td");
      if (cell) highlight(cell);
    });
    matrix.addEventListener("mouseleave", reset);
    matrix.addEventListener("focusout", (event) => {
      if (!matrix.contains(event.relatedTarget)) reset();
    });
  }

  /* ------------------------------------------------------------- screens */
  const shotTabs = $$(".shot-tab");
  const shotImg = $("#shot-img");
  const shotCaption = $("#shot-caption");
  const shotPage = $("#shot-page");
  const lightbox = $("#lightbox");
  const lightboxImg = $("#lightbox-img");
  const lightboxCaption = $("#lightbox-caption");
  let shotIndex = 0;

  const selectShot = (index, focus = false) => {
    if (!shotTabs.length || !shotImg) return;
    shotIndex = (index + shotTabs.length) % shotTabs.length;
    const tab = shotTabs[shotIndex];
    const label = tab.textContent.trim();
    shotTabs.forEach((btn, i) => {
      const active = i === shotIndex;
      btn.classList.toggle("is-active", active);
      btn.setAttribute("aria-selected", String(active));
      btn.tabIndex = active ? 0 : -1;
    });
    if (focus) tab.focus();

    shotImg.classList.add("is-swapping");
    shotImg.src = `assets/images/${tab.dataset.shot}.png`;
    shotImg.alt = `控制台${label}页面`;
    const done = () => shotImg.classList.remove("is-swapping");
    if (shotImg.complete) {
      window.requestAnimationFrame(done);
    } else {
      shotImg.addEventListener("load", done, { once: true });
      shotImg.addEventListener("error", done, { once: true });
    }
    if (shotCaption) shotCaption.textContent = tab.dataset.caption || label;
    if (shotPage) shotPage.textContent = label;
  };

  if (shotTabs.length) {
    shotTabs.forEach((tab, i) => {
      tab.addEventListener("click", () => selectShot(i));
      tab.addEventListener("keydown", (event) => {
        if (event.key === "ArrowRight") {
          event.preventDefault();
          selectShot(i + 1, true);
        } else if (event.key === "ArrowLeft") {
          event.preventDefault();
          selectShot(i - 1, true);
        }
      });
    });
    selectShot(0);

    const preload = () => {
      shotTabs.slice(1).forEach((tab) => {
        const img = new Image();
        img.src = `assets/images/${tab.dataset.shot}.png`;
      });
    };
    if ("requestIdleCallback" in window) {
      window.requestIdleCallback(preload, { timeout: 3000 });
    } else {
      window.setTimeout(preload, 1400);
    }
  }

  const shotOpen = $("#shot-open");
  if (shotOpen && lightbox && shotImg && typeof lightbox.showModal === "function") {
    shotOpen.addEventListener("click", () => {
      lightboxImg.src = shotImg.src;
      lightboxImg.alt = shotImg.alt;
      if (lightboxCaption) lightboxCaption.textContent = shotCaption ? shotCaption.textContent : "";
      lightbox.showModal();
    });
  } else if (shotOpen && shotImg) {
    shotOpen.addEventListener("click", () => window.open(shotImg.src, "_blank", "noopener"));
  }

  if (lightbox) {
    const closeBtn = $("#lightbox-close");
    if (closeBtn) closeBtn.addEventListener("click", () => lightbox.close());
    lightbox.addEventListener("click", (event) => {
      if (event.target === lightbox) lightbox.close();
    });
  }

  /* ---------------------------------------------------------- start tabs */
  $$("[data-tabs]").forEach((group) => {
    const scope = group.dataset.tabs;
    const panels = $(`[data-panels="${scope}"]`);
    if (!panels) return;
    const buttons = $$("button[data-tab]", group);
    buttons.forEach((btn) => {
      btn.addEventListener("click", () => {
        buttons.forEach((other) => {
          const active = other === btn;
          other.classList.toggle("is-active", active);
          other.setAttribute("aria-selected", String(active));
        });
        $$("[data-panel]", panels).forEach((panel) => {
          const active = panel.dataset.panel === btn.dataset.tab;
          panel.classList.toggle("is-active", active);
          panel.hidden = !active;
        });
      });
    });
  });

  /* --------------------------------------------------------------- copy */
  const legacyCopy = (text) => {
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.top = "-1000px";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    try {
      document.execCommand("copy");
    } catch {
      /* ignore */
    }
    area.remove();
  };

  $$("[data-copy]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const source = document.getElementById(btn.dataset.copy);
      if (!source) return;
      const text = source.textContent.trim();
      try {
        if (navigator.clipboard && window.isSecureContext) {
          await navigator.clipboard.writeText(text);
        } else {
          legacyCopy(text);
        }
      } catch {
        legacyCopy(text);
      }
      const original = btn.textContent;
      btn.textContent = "已复制";
      btn.classList.add("is-done");
      window.setTimeout(() => {
        btn.textContent = original;
        btn.classList.remove("is-done");
      }, 1600);
    });
  });

  /* ------------------------------------------------------------- releases */
  const REPO = document.body.dataset.repo || "yangyongyongyong/llm-protocol-gateway";
  const releaseVersion = $("#release-version");
  const releaseState = $("#release-state");
  const releaseAll = $("#release-all");
  const releasesPage = `https://github.com/${REPO}/releases`;

  if (releaseAll) releaseAll.href = releasesPage;

  const formatSize = (bytes) => {
    if (!Number.isFinite(bytes) || bytes <= 0) return "";
    if (bytes >= 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024 / 1024).toFixed(1)} GB`;
    if (bytes >= 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
    return `${Math.max(1, Math.round(bytes / 1024))} KB`;
  };

  const cacheKey = `lpg:release:${REPO}`;

  const readCache = () => {
    try {
      const raw = sessionStorage.getItem(cacheKey);
      if (!raw) return null;
      const parsed = JSON.parse(raw);
      if (!parsed || Date.now() - parsed.at > 10 * 60 * 1000) return null;
      return parsed.data;
    } catch {
      return null;
    }
  };

  const writeCache = (data) => {
    try {
      sessionStorage.setItem(cacheKey, JSON.stringify({ at: Date.now(), data }));
    } catch {
      /* storage full or disabled */
    }
  };

  const loadRelease = async () => {
    const cached = readCache();
    if (cached) return cached;
    const res = await fetch(`https://api.github.com/repos/${REPO}/releases/latest`, {
      headers: { Accept: "application/vnd.github+json" },
    });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const json = await res.json();
    const data = {
      tag_name: json.tag_name,
      published_at: json.published_at,
      html_url: json.html_url,
      assets: Array.isArray(json.assets)
        ? json.assets.map((asset) => ({
            name: asset.name,
            size: asset.size,
            browser_download_url: asset.browser_download_url,
            download_count: asset.download_count,
          }))
        : [],
    };
    writeCache(data);
    return data;
  };

  const applyRelease = (data) => {
    const tag = data.tag_name || "";
    if (tag && releaseVersion) releaseVersion.textContent = tag;
    if (data.html_url && releaseAll) releaseAll.href = data.html_url;

    const assets = data.assets || [];
    const links = $$("[data-asset]");
    let matched = 0;

    links.forEach((link) => {
      const suffix = link.dataset.asset;
      const asset = assets.find((item) => item.name.endsWith(suffix));
      if (!asset) return;
      matched += 1;
      link.href = asset.browser_download_url;
      link.title = `${asset.name} · ${formatSize(asset.size)}`;
      const sizeSlot = $(".dl-size", link);
      if (sizeSlot) sizeSlot.textContent = formatSize(asset.size);
      if (link.closest(".cli-table")) {
        link.setAttribute("aria-label", `下载 ${asset.name}`);
      }
    });

    if (!releaseState) return;
    releaseState.classList.remove("is-ready");
    if (!assets.length) {
      releaseState.textContent = "该版本未附带安装包，请前往 Releases 选择构建版本";
    } else {
      const when = data.published_at
        ? new Date(data.published_at).toLocaleDateString("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit" })
        : "";
      const detail = matched ? `已同步 ${matched} 个下载链接` : `${assets.length} 个文件`;
      releaseState.textContent = when ? `发布于 ${when} · ${detail}` : detail;
      releaseState.classList.add("is-ready");
    }
  };

  const failRelease = () => {
    if (!releaseState) return;
    releaseState.textContent = "Releases API 暂时不可用，下载按钮会跳转到 Releases 页面";
    $$("[data-asset]").forEach((link) => {
      if (!link.getAttribute("href") || link.getAttribute("href").includes("/releases")) {
        link.href = releasesPage;
      }
    });
  };

  if (releaseVersion) {
    loadRelease().then(applyRelease).catch(failRelease);
  }

  /* ------------------------------------------------------------ star count */
  const starSlot = $("[data-star-count]");
  if (starSlot) {
    fetch(`https://api.github.com/repos/${REPO}`, {
      headers: { Accept: "application/vnd.github+json" },
    })
      .then((res) => (res.ok ? res.json() : null))
      .then((data) => {
        if (!data || !Number.isFinite(data.stargazers_count) || data.stargazers_count <= 0) return;
        try {
          starSlot.textContent = new Intl.NumberFormat("en", {
            notation: "compact",
            maximumFractionDigits: 1,
          }).format(data.stargazers_count);
        } catch {
          starSlot.textContent = String(data.stargazers_count);
        }
        starSlot.hidden = false;
      })
      .catch(() => {});
  }

  /* --------------------------------------------------------- OS detection */
  const platform =
    (navigator.userAgentData && navigator.userAgentData.platform) ||
    navigator.platform ||
    navigator.userAgent ||
    "";
  const mobile = /iPhone|iPad|iPod|Android/i.test(navigator.userAgent);
  let currentOS = "";
  if (!mobile) {
    if (/mac/i.test(platform)) currentOS = "mac";
    else if (/win/i.test(platform)) currentOS = "win";
    else if (/linux|x11/i.test(platform)) currentOS = "linux";
  }
  if (currentOS) {
    const card = $(`.dl-card[data-os="${currentOS}"]`);
    if (card) {
      card.classList.add("is-current");
      const badge = $("[data-os-badge]", card);
      if (badge) badge.hidden = false;
    }
  }
})();
