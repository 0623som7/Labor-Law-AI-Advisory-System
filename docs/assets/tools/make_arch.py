# 生成的产物：docs/assets/architecture.png
# 改产物请改本脚本后重跑，不要手改 PNG / pptx
# 运行方式：在仓库根目录执行 python docs/assets/tools/make_arch.py
"""按答辩 P4 规范绘制架构图：竖向三段 + 右侧模型列（2400x1350，16:9）。

为什么不用 mermaid 直接出图：mermaid-cli 12.2.0 忽略 subgraph 内的 direction，
实测「每段横排 + 右侧单列」拿不到（dagre 一律按全局方向堆叠）。这里按规范逐像素绘制；
docs/assets/architecture.mmd 保留同内容的 mermaid 源，需要时可单独渲染。
"""
import math
import os
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[3]  # docs/assets/tools/make_arch.py → 仓库根
OUT = ROOT / "docs" / "assets" / "architecture.png"

S = 1.5
W, H = int(2400 * S), int(1350 * S)
NAVY = (31, 58, 95)
INK = (27, 42, 65)
WHITE = (255, 255, 255)

# 系统字体目录：Windows 自带微软雅黑；非 Windows 环境可用 ARCH_FONT_DIR 指到同名字体所在目录。
FD = os.environ.get("ARCH_FONT_DIR", "C:/Windows/Fonts/")


def font(size, bold=False):
    return ImageFont.truetype(FD + ("msyhbd.ttc" if bold else "msyh.ttc"), int(size * S))


F_BADGE = font(20, True)
F_TITLE = font(22, True)
F_NAME = font(23, True)
F_DESC = font(18)
F_NUM = font(32, True)
F_LABEL = font(19, True)

img = Image.new("RGB", (W, H), WHITE)
d = ImageDraw.Draw(img)


def box(x0, y0, x1, y1, fill, border=NAVY, width=2, radius=12):
    d.rounded_rectangle([x0 * S, y0 * S, x1 * S, y1 * S], radius=radius * S,
                        fill=fill, outline=border, width=max(1, int(width * S)))


def text(x, y, s, f, fill=INK, center_x=None):
    if center_x is None:
        d.text((x * S, y * S), s, font=f, fill=fill)
    else:
        w = d.textlength(s, font=f)
        d.text(((center_x * S) - w / 2, y * S), s, font=f, fill=fill)


def badge(x, y, mark):
    box(x, y, x + 33, y + 29, NAVY, NAVY, 1, 7)
    text(x + 16.5, y + 3.5, mark, F_BADGE, WHITE, center_x=x + 16.5)


def card(x0, y0, x1, y1, name, desc=None, big=None, fill=WHITE, mark=None):
    box(x0, y0, x1, y1, fill, NAVY, 1.2, 9)
    if mark:
        badge(x0 + 14, y0 + 12, mark)
    lines = [(name, F_NAME)]
    if big:
        lines.append((big, F_NUM))
    if desc:
        lines += [(p, F_DESC) for p in desc.split("\n")]
    heights = [f.size * 1.32 for _, f in lines]
    y = (y0 + y1) / 2 - (sum(heights) / S) / 2 + 10
    for (t, f), h in zip(lines, heights):
        text(0, y, t, f, INK, center_x=(x0 + x1) / 2)
        y += h / S


def line(a, b, dashed=False):
    (x0, y0), (x1, y1) = (a[0] * S, a[1] * S), (b[0] * S, b[1] * S)
    if dashed:
        dist = math.hypot(x1 - x0, y1 - y0)
        if dist == 0:
            return
        dx, dy = (x1 - x0) / dist, (y1 - y0) / dist
        t = 0.0
        while t < dist:
            t2 = min(t + 9 * S, dist)
            d.line([(x0 + dx * t, y0 + dy * t), (x0 + dx * t2, y0 + dy * t2)], fill=NAVY, width=int(1.5 * S))
            t = t2 + 7 * S
    else:
        d.line([(x0, y0), (x1, y1)], fill=NAVY, width=int(1.8 * S))


def arrow(points, dashed=False, label=None, label_xy=None):
    for i in range(len(points) - 1):
        line(points[i], points[i + 1], dashed)
    (ax, ay), (bx, by) = points[-2], points[-1]
    ang = math.atan2((by - ay) * S, (bx - ax) * S)
    L, Wd = 13 * S, 5.2 * S
    tip = (bx * S, by * S)
    left = (bx * S - L * math.cos(ang) + Wd * math.sin(ang), by * S - L * math.sin(ang) - Wd * math.cos(ang))
    right = (bx * S - L * math.cos(ang) - Wd * math.sin(ang), by * S - L * math.sin(ang) + Wd * math.cos(ang))
    d.polygon([tip, left, right], fill=NAVY)
    if label and label_xy:
        tw = d.textlength(label, font=F_LABEL) / S
        cx, cy = label_xy
        d.rectangle([(cx - tw / 2 - 6) * S, (cy - 3) * S, (cx + tw / 2 + 6) * S, (cy + 24) * S], fill=WHITE)
        text(0, cy, label, F_LABEL, NAVY, center_x=cx)


# ── 版面 ───────────────────────────────────────────────────────────────
LX0, LX1 = 40, 1720
MX0, MX1 = 1880, 2360

# 第一段：① 浏览器 → ② nginx
box(LX0, 40, LX1, 330, WHITE, NAVY, 2, 14)
card(90, 108, 690, 278, "浏览器 · Vue3 + TS", "上传页 / 审查页 / 对话页", fill="#F5F7FA", mark="①")
card(880, 108, 1670, 278, "nginx 1.27", "443 终止 · /api 反向代理 · 静态资源托管", fill="#EEF2F7", mark="②")
arrow([(690, 165), (880, 165)], label="443", label_xy=(785, 128))
arrow([(690, 235), (880, 235)], dashed=True, label="8443 兜底", label_xy=(785, 244))

# 第二段：③ 应用层
box(LX0, 370, LX1, 790, "#E8EEF6", NAVY, 2, 14)
badge(LX0 + 16, 382, "③")
text(LX0 + 60, 384, "应用层 · Go + Gin", F_TITLE, NAVY)

mods = [("会话鉴权", "JWT 签发校验\n双桶限流"),
        ("对话链路", "检索 → 提示词\n→ 流式输出"),
        ("审查链路", "21 条规则\n→ 坐标回填"),
        ("worker", "异步审查任务\n重启不清零")]
mx, mw, mgap, my0, my1 = 70, 383, 26, 435, 665
for i, (n, ds) in enumerate(mods):
    x0 = mx + i * (mw + mgap)
    card(x0, my0, x0 + mw, my1, n, ds)

# 第三段：④ 数据层
box(LX0, 830, LX1, 1300, "#F2F5F3", NAVY, 2, 14)
badge(LX0 + 16, 842, "④")
text(LX0 + 60, 844, "数据层 · 自建 Docker", F_TITLE, NAVY)
card(90, 900, 720, 1190, "MySQL 8", "会话 / 审查 / 向量", big="596 条法条")
card(790, 900, 1400, 1190, "Redis", "限流桶 · 向量缓存")

# 右侧：⑤ 外部模型列
box(MX0, 370, MX1, 1300, "#F7F4EE", NAVY, 2, 14)
badge(MX0 + 16, 382, "⑤")
text(0, 384, "外部模型 · DashScope", F_TITLE, NAVY, center_x=(MX0 + MX1) / 2)
card(MX0 + 30, 430, MX1 - 30, 660, "qwen-plus", "对话生成")
card(MX0 + 30, 760, MX1 - 30, 990, "text-embedding-v3", "1024 维向量")

# ── 连线（5 个标签：443、8443 兜底、/api、生成、向量召回）────────────────
arrow([(1200, 330), (1200, 370)], label="/api", label_xy=(1237, 336))
arrow([(955, 790), (955, 830)])
arrow([(1720, 480), (1880, 480)], label="生成", label_xy=(1800, 444))
arrow([(1720, 620), (1800, 620), (1800, 880), (1880, 880)], label="向量召回", label_xy=(1800, 740))

OUT.parent.mkdir(parents=True, exist_ok=True)
img.resize((2400, 1350), Image.LANCZOS).save(str(OUT))
print("saved", OUT, "2400x1350")
