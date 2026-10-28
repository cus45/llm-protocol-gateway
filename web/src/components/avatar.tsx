import React, {useRef, useState} from 'react';

/**
 * 头像上传的共用逻辑：用户编辑弹窗 / 个人资料弹窗都走这套，避免两份裁剪代码。
 *
 * 后端只接受不超过 512KB 的 base64 位图 data URL（png/jpeg/webp/gif）或 / 开头的
 * 路径，SVG 会被拒。手机直出的照片动辄几 MB，所以这里在浏览器里用 canvas 居中裁成
 * 正方形、缩到最长边 256px 再编码；编码结果仍超过上限时逐级降尺寸/画质重试。
 */

const ACCEPT = 'image/png,image/jpeg,image/webp,image/gif';

// 后端上限是 512KB 的 base64 长度；这里再留一点余量给 data URL 前缀。
const MAX_BASE64_LENGTH = 500 * 1024;

const MAX_PIXEL_SIDE = 256;

/** 出现这个值的文件名说明用户选的是 SVG/HEIC 之类解码不了的格式。 */
export class AvatarError extends Error {}

function readFileAsDataURL(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result || ''));
    reader.onerror = () => reject(new AvatarError('读取图片失败'));
    reader.readAsDataURL(file);
  });
}

function loadImage(src: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const image = new Image();
    image.onload = () => resolve(image);
    image.onerror = () => reject(new AvatarError('无法识别的图片格式，请换一张 PNG/JPEG/WebP/GIF 图片'));
    image.src = src;
  });
}

function canvasToDataURL(canvas: HTMLCanvasElement, type: string, quality: number): string {
  const dataUrl = canvas.toDataURL(type, quality);
  // 个别浏览器不支持 webp 编码时会悄悄退回 png；后端只收白名单格式，这里兜底。
  if (!/^data:image\/(png|jpeg|webp|gif);base64,/.test(dataUrl)) {
    return canvas.toDataURL('image/png');
  }
  return dataUrl;
}

/** 裁剪 + 缩放 + 编码。单张静态图；返回可直接提交的 data URL。 */
async function downscale(dataUrl: string): Promise<string> {
  const image = await loadImage(dataUrl);
  const side = Math.min(image.naturalWidth || image.width, image.naturalHeight || image.height);
  if (!side) throw new AvatarError('图片尺寸无效');
  const sourceX = ((image.naturalWidth || image.width) - side) / 2;
  const sourceY = ((image.naturalHeight || image.height) - side) / 2;
  const output = document.createElement('canvas');
  // 带透明通道的 PNG 编码成 JPEG 会出现黑底，先探测一次是否真有透明像素。
  const probe = document.createElement('canvas');
  probe.width = 1;
  probe.height = 1;
  const probeCtx = probe.getContext('2d');
  let hasAlpha = false;
  if (probeCtx) {
    probeCtx.drawImage(image, 0, 0, 1, 1);
    hasAlpha = probeCtx.getImageData(0, 0, 1, 1).data[3] < 250;
  }
  const sizes = [MAX_PIXEL_SIDE, 192, 160, 128, 96];
  const qualities = hasAlpha ? [] : [0.88, 0.78, 0.66, 0.55];
  const types = hasAlpha ? ['image/png'] : ['image/jpeg', 'image/webp'];
  for (const size of sizes) {
    output.width = size;
    output.height = size;
    const ctx = output.getContext('2d');
    if (!ctx) throw new AvatarError('浏览器不支持图片处理');
    ctx.clearRect(0, 0, size, size);
    if (!hasAlpha) {
      // JPEG 无透明通道：先铺主题无关的白底，避免边角残留黑/透明色。
      ctx.fillStyle = '#ffffff';
      ctx.fillRect(0, 0, size, size);
    }
    ctx.imageSmoothingEnabled = true;
    ctx.imageSmoothingQuality = 'high';
    ctx.drawImage(image, sourceX, sourceY, side, side, 0, 0, size, size);
    const attempts: Array<{ type: string; quality: number }> = hasAlpha
      ? types.map((type) => ({ type, quality: 1 }))
      : qualities.flatMap((quality) => types.map((type) => ({ type, quality })));
    for (const attempt of attempts) {
      const encoded = canvasToDataURL(output, attempt.type, attempt.quality);
      if (encoded.length <= MAX_BASE64_LENGTH) return encoded;
    }
  }
  throw new AvatarError('图片压缩后仍超过 512KB，请换一张更小的图片');
}

/**
 * 把用户选的图片文件转成后端可接受的头像 data URL。
 * GIF 若是动画会丢掉动画帧（canvas 只能取第一帧），静态图不受影响。
 */
export async function fileToAvatarDataURL(file: File): Promise<string> {
  if (!/^image\/(png|jpeg|webp|gif)$/.test(file.type)) {
    throw new AvatarError('仅支持 PNG / JPEG / WebP / GIF 图片');
  }
  if (file.size > 20 * 1024 * 1024) {
    throw new AvatarError('图片过大（超过 20MB）');
  }
  return downscale(await readFileAsDataURL(file));
}

/** 头像展示：有图用图，没有就用名字首字（与侧栏 user-avatar 的观感一致）。 */
export function Avatar({ src, name, size = 36, className = '' }: {
  src?: string;
  name: string;
  size?: number;
  className?: string;
}) {
  const [broken, setBroken] = useState(false);
  const style = { width: size, height: size, borderRadius: Math.max(8, Math.round(size / 4.5)), fontSize: Math.round(size * 0.4) };
  if (src && !broken) {
    return (
      <img
        className={`rbac-avatar ${className}`}
        style={style}
        src={src}
        alt=""
        onError={() => setBroken(true)}
      />
    );
  }
  return (
    <span className={`rbac-avatar fallback ${className}`} style={style} aria-hidden="true">
      {initialOf(name)}
    </span>
  );
}

export function initialOf(name: string): string {
  return (name || '').trim().slice(0, 1).toUpperCase() || '?';
}

/**
 * 头像编辑控件：预览 + 上传/更换 + 清除。
 * 父组件持有 value（data URL 或 / 路径），这里只负责选图与转换。
 */
export function AvatarUploader({ value, name, onChange, disabled }: {
  value: string;
  name: string;
  onChange: (next: string) => void;
  disabled?: boolean;
}) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  async function pick(event: React.ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    // 清空 value，允许重复选择同一个文件时也能触发 change。
    event.target.value = '';
    if (!file) return;
    setBusy(true);
    setError('');
    try {
      onChange(await fileToAvatarDataURL(file));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="rbac-avatar-uploader">
      <Avatar src={value} name={name} size={56} />
      <div className="rbac-avatar-uploader-body">
        <div className="rbac-avatar-uploader-actions">
          <button
            type="button"
            className="mini-btn"
            disabled={disabled || busy}
            onClick={() => inputRef.current?.click()}
          >
            {busy ? '处理中…' : value ? '更换头像' : '上传头像'}
          </button>
          {value ? (
            <button
              type="button"
              className="mini-btn"
              disabled={disabled || busy}
              onClick={() => {
                setError('');
                onChange('');
              }}
            >
              清除
            </button>
          ) : null}
        </div>
        <div className="hint-text">支持 PNG / JPEG / WebP / GIF，会自动裁剪为正方形并压缩到 256px 以内。</div>
        {error ? <div className="rbac-form-error">{error}</div> : null}
      </div>
      <input
        ref={inputRef}
        type="file"
        accept={ACCEPT}
        hidden
        onChange={(event) => void pick(event)}
      />
    </div>
  );
}
