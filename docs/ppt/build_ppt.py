# -*- coding: utf-8 -*-
# 生成的产物：docs/ppt/答辩.pptx（docs/ppt/自检/slide-*.png 由它渲染）
# 自检图（docs/ppt/自检/slide-*.png）需手工用 LibreOffice Kit 渲染，本脚本不生成
# 改产物请改本脚本后重跑，不要手改 PNG / pptx
# 运行方式：在仓库根目录执行 python docs/ppt/build_ppt.py
"""生成答辩 PPT（12 页，16:9）。数字只取白名单，不新增任何指标。"""
from pathlib import Path

from pptx import Presentation
from pptx.chart.data import CategoryChartData
from pptx.dml.color import RGBColor
from pptx.enum.chart import XL_CHART_TYPE, XL_LABEL_POSITION
from pptx.enum.text import MSO_ANCHOR, PP_ALIGN
from pptx.oxml import parse_xml
from pptx.oxml.ns import nsdecls, qn
from pptx.util import Inches, Pt

ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / "docs" / "ppt" / "答辩.pptx"
ARCH = ROOT / "docs" / "assets" / "architecture.png"
SHOT_REVIEW = ROOT / "docs" / "assets" / "shots" / "4-review.png"

NAVY = RGBColor(0x1F, 0x3A, 0x5F)
GRAY = RGBColor(0x4B, 0x55, 0x63)
RED = RGBColor(0xD9, 0x30, 0x26)
WHITE = RGBColor(0xFF, 0xFF, 0xFF)
LIGHT = RGBColor(0xF5, 0xF7, 0xFA)
FONT = "微软雅黑"


def style_run(run, size, bold=False, color=GRAY, font=FONT):
    run.font.size = Pt(size)
    run.font.bold = bold
    run.font.color.rgb = color
    run.font.name = font
    rPr = run._r.get_or_add_rPr()
    ea = rPr.find(qn("a:ea"))
    if ea is None:
        ea = parse_xml('<a:ea %s typeface="%s"/>' % (nsdecls("a"), font))
        rPr.append(ea)
    else:
        ea.set("typeface", font)


def textbox(slide, left, top, width, height, lines, size=22, color=GRAY,
            bold=False, align=PP_ALIGN.LEFT, spacing=1.35, anchor=MSO_ANCHOR.TOP):
    box = slide.shapes.add_textbox(Inches(left), Inches(top), Inches(width), Inches(height))
    tf = box.text_frame
    tf.word_wrap = True
    tf.vertical_anchor = anchor
    if isinstance(lines, str):
        lines = [lines]
    for i, item in enumerate(lines):
        if isinstance(item, tuple):
            text, opts = item
        else:
            text, opts = item, {}
        p = tf.paragraphs[0] if i == 0 else tf.add_paragraph()
        p.alignment = opts.get("align", align)
        p.line_spacing = opts.get("spacing", spacing)
        if opts.get("space_before"):
            p.space_before = Pt(opts["space_before"])
        run = p.add_run()
        run.text = text
        style_run(run, opts.get("size", size), opts.get("bold", bold), opts.get("color", color))
    return box


def add_source(slide, text):
    box = slide.shapes.add_textbox(Inches(7.9), Inches(6.95), Inches(4.9), Inches(0.3))
    tf = box.text_frame
    tf.word_wrap = True
    p = tf.paragraphs[0]
    p.alignment = PP_ALIGN.RIGHT
    run = p.add_run()
    run.text = "数据出处：" + text
    style_run(run, 10, False, RGBColor(0x9A, 0xA3, 0xAE))


def add_title(slide, text, size=32, top=0.42):
    textbox(slide, 0.72, top, 11.9, 0.9, [(text, {"size": size, "bold": True, "color": NAVY})])


def add_rule(slide, top=1.28):
    line = slide.shapes.add_shape(1, Inches(0.75), Inches(top), Inches(11.8), Inches(0.03))
    line.fill.solid()
    line.fill.fore_color.rgb = NAVY
    line.line.fill.background()
    line.shadow.inherit = False
    return line


def card(slide, left, top, width, height, fill=LIGHT, line=None):
    shape = slide.shapes.add_shape(5, Inches(left), Inches(top), Inches(width), Inches(height))
    shape.fill.solid()
    shape.fill.fore_color.rgb = fill
    if line is None:
        shape.line.fill.background()
    else:
        shape.line.color.rgb = line
        shape.line.width = Pt(1)
    shape.shadow.inherit = False
    shape.text_frame.word_wrap = True
    return shape


def blank(prs):
    return prs.slides.add_slide(prs.slide_layouts[6])


def main():
    prs = Presentation()
    prs.slide_width = Inches(13.333)
    prs.slide_height = Inches(7.5)

    # ---------- P1 封面 ----------
    s = blank(prs)
    bar = s.shapes.add_shape(1, Inches(0), Inches(0), Inches(13.333), Inches(0.18))
    bar.fill.solid(); bar.fill.fore_color.rgb = NAVY; bar.line.fill.background(); bar.shadow.inherit = False
    textbox(s, 0.9, 1.75, 11.5, 1.1, [("劳动合同风险审查与劳动法问答系统", {"size": 40, "bold": True, "color": NAVY})])
    textbox(s, 0.9, 3.0, 11.5, 0.8, [("基于规则匹配与混合检索的劳动合同合规辅助工具", {"size": 24, "color": GRAY})])
    line = s.shapes.add_shape(1, Inches(0.95), Inches(4.05), Inches(3.2), Inches(0.04))
    line.fill.solid(); line.fill.fore_color.rgb = NAVY; line.line.fill.background(); line.shadow.inherit = False
    textbox(s, 0.9, 4.35, 11.5, 0.6, [("25 级软工 2 班 · 梁凯琳 · 2025404030514", {"size": 22, "color": GRAY})])
    textbox(s, 0.9, 5.05, 11.5, 0.5, [("答辩 2026-12-06", {"size": 18, "color": RGBColor(0x9A, 0xA3, 0xAE)})])

    # ---------- P2 要解决的问题 ----------
    s = blank(prs)
    add_title(s, "要解决的问题")
    add_rule(s)
    textbox(s, 0.95, 2.0, 11.4, 3.6, [
        ("· 劳动者签了合同，不知道自己签了什么", {"size": 24, "color": GRAY}),
        ("· 通用大模型会编法条，编得还很像", {"size": 24, "color": GRAY, "space_before": 26}),
        ("· 律师咨询贵、慢，且不在大多数人的选项里", {"size": 24, "color": GRAY, "space_before": 26}),
    ], spacing=1.5)
    add_source(s, "项目动机（问题定义）")

    # ---------- P3 我们做了什么 ----------
    s = blank(prs)
    add_title(s, "我们做了什么")
    add_rule(s)
    textbox(s, 0.95, 1.6, 11.4, 0.9, [("把「合同里哪一条违法」变成「第几条第几页第几行，违反哪一条法」", {"size": 22, "bold": True, "color": NAVY})])
    items = [
        ("能力 1 · 合同风险审查", "21 条规则，覆盖 7 类高频违法点"),
        ("能力 2 · 劳动法问答", "596 条法条，答必带原文引用"),
        ("能力 3 · 可定位", "结论能点回 PDF 原位置"),
    ]
    for i, (t, d) in enumerate(items):
        top = 2.75 + i * 1.25
        card(s, 0.95, top, 11.4, 1.05)
        textbox(s, 1.15, top + 0.12, 4.2, 0.5, [(t, {"size": 22, "bold": True, "color": NAVY})])
        textbox(s, 5.3, top + 0.16, 6.8, 0.6, [(d, {"size": 20, "color": GRAY})])
    add_source(s, "系统能力（规则 21 条 / 法条 596 条）")

    # ---------- P4 架构图（恢复页面标题；竖排三段 + 右侧模型列版） ----------
    s = blank(prs)
    textbox(s, 0.866, 0.24, 11.6, 0.46,
            [("系统架构", {"size": 28, "bold": True, "color": NAVY})],
            anchor=MSO_ANCHOR.MIDDLE)
    # 11.6 : 6.525 = 1.7778，与 architecture.png 的 2400:1350 一致，不拉伸
    s.shapes.add_picture(str(ARCH), Inches(0.866), Inches(0.80), Inches(11.6), Inches(6.525))
    cap = s.shapes.add_textbox(Inches(0.866), Inches(7.34), Inches(11.6), Inches(0.14))
    cap_tf = cap.text_frame
    cap_tf.word_wrap = False
    cap_tf.margin_left = 0
    cap_tf.margin_right = 0
    cap_tf.margin_top = 0
    cap_tf.margin_bottom = 0
    cap_tf.vertical_anchor = MSO_ANCHOR.MIDDLE
    cap_p = cap_tf.paragraphs[0]
    cap_p.alignment = PP_ALIGN.LEFT
    # 10pt 字行高约 0.185in，而框高只有 0.14in：把行距锁成 10pt 精确值，
    # 让整行落在 7.34–7.48 之间，避免底边被版心裁掉。
    cap_p.line_spacing = Pt(10)
    cap_run = cap_p.add_run()
    cap_run.text = "数据出处：deploy/ 与 internal/ 实际代码"
    style_run(cap_run, 10, False, RGBColor(0x6B, 0x72, 0x80))

    # ---------- P5 数据层 ----------
    s = blank(prs)
    add_title(s, "数据层：596 条法条从哪来")
    add_rule(s)
    textbox(s, 0.95, 1.65, 7.2, 1.2, [("596 条 · 11 部法律法规", {"size": 32, "bold": True, "color": NAVY})])
    textbox(s, 0.95, 2.55, 7.0, 0.7, [("脚本化入库（新法 20 分钟一部）", {"size": 22, "color": GRAY})])
    textbox(s, 0.95, 3.5, 7.0, 2.6, [
        ("劳动合同法 98 / 劳动法 107 / 实施条例 38", {"size": 14, "color": RGBColor(0x6B, 0x72, 0x80)}),
        ("工伤保险 67 / 社会保险 98 / 调解仲裁 54", {"size": 14, "color": RGBColor(0x6B, 0x72, 0x80)}),
        ("劳务派遣 29 / 女职工 16 / 年休假 10", {"size": 14, "color": RGBColor(0x6B, 0x72, 0x80)}),
        ("最低工资 15 / 农民工工资 64", {"size": 14, "color": RGBColor(0x6B, 0x72, 0x80)}),
    ], spacing=1.35)
    # 右侧法条卡片示意
    card(s, 8.35, 1.75, 4.2, 3.5, fill=WHITE, line=RGBColor(0xD5, 0xDB, 0xE3))
    textbox(s, 8.6, 1.95, 3.7, 0.4, [("社会保险法 · 第六十三条", {"size": 20, "bold": True, "color": NAVY})])
    textbox(s, 8.6, 2.45, 3.7, 2.6, [("用人单位未按时足额缴纳社会保险费的，由社会保险费征收机构责令其限期缴纳或者补足。", {"size": 16, "color": GRAY})])
    add_source(s, "数据库 statutes 表 596 行 / 11 部")

    # ---------- P6 检索四档 ----------
    s = blank(prs)
    add_title(s, "检索：四档实测对比（@3 命中率）")
    add_rule(s)
    data = CategoryChartData()
    data.categories = ["纯词法", "纯向量", "融合（线上）", "融合 + 重排"]
    data.add_series("hit@3", (97.3, 86.6, 98.2, 96.4))
    frame = s.shapes.add_chart(XL_CHART_TYPE.COLUMN_CLUSTERED, Inches(0.85), Inches(1.55), Inches(7.3), Inches(3.6), data)
    chart = frame.chart
    plot = chart.plots[0]
    plot.has_data_labels = True
    dl = plot.data_labels
    dl.number_format = '0.0"%"'
    dl.number_format_is_linked = False
    dl.position = XL_LABEL_POSITION.OUTSIDE_END
    dl.font.size = Pt(16)
    dl.font.color.rgb = NAVY
    dl.font.name = FONT
    chart.has_legend = False
    chart.has_title = False
    ser = plot.series[0]
    ser.format.fill.solid()
    ser.format.fill.fore_color.rgb = RGBColor(0x9D, 0xB2, 0xCC)
    ser.points[2].format.fill.solid()
    ser.points[2].format.fill.fore_color.rgb = NAVY
    chart.category_axis.tick_labels.font.size = Pt(16)
    chart.category_axis.tick_labels.font.name = FONT
    chart.category_axis.tick_labels.font.color.rgb = GRAY
    chart.value_axis.maximum_scale = 110
    chart.value_axis.minimum_scale = 0
    chart.value_axis.has_major_gridlines = False
    chart.value_axis.tick_labels.font.size = Pt(12)
    chart.value_axis.tick_labels.font.color.rgb = RGBColor(0x9A, 0xA3, 0xAE)

    textbox(s, 8.5, 1.62, 4.1, 3.5, [
        ("线上用融合档 98.2%", {"size": 24, "bold": True, "color": NAVY}),
        ("做法：词表召回 + 向量召回 → RRF 融合 → 交叉编码器重排（测过未启用）", {"size": 16, "color": GRAY, "space_before": 18}),
        ("为什么不用重排：救回 1 条、压掉 3 条，净 −2", {"size": 16, "color": GRAY, "space_before": 18}),
    ], spacing=1.3)
    add_source(s, "eval 评测集 112 条，四档实测")

    # ---------- P7 审查为什么不调大模型 ----------
    s = blank(prs)
    add_title(s, "审查：为什么不把合同喂给大模型")
    add_rule(s)
    card(s, 0.85, 1.65, 5.6, 3.9, fill=RGBColor(0xF3, 0xF4, 0xF6))
    textbox(s, 1.1, 1.85, 5.1, 0.45, [("常规做法", {"size": 24, "bold": True, "color": GRAY})])
    textbox(s, 1.1, 2.45, 5.1, 2.9, [
        ("整份合同喂给模型", {"size": 20, "color": GRAY}),
        ("↓ 要素抽取", {"size": 20, "color": GRAY, "space_before": 14}),
        ("↓ 判定（几十秒，且可能编）", {"size": 20, "color": GRAY, "space_before": 14}),
    ], spacing=1.25)
    card(s, 6.9, 1.65, 5.6, 3.9, fill=WHITE, line=NAVY)
    textbox(s, 7.15, 1.85, 5.1, 0.45, [("我们的做法", {"size": 24, "bold": True, "color": NAVY})])
    textbox(s, 7.15, 2.45, 5.1, 2.9, [
        ("21 条规则在原文上匹配", {"size": 20, "color": GRAY}),
        ("↓ 坐标从版面块回填", {"size": 20, "color": GRAY, "space_before": 14}),
        ("↓ 58 毫秒，结论可复现", {"size": 20, "bold": True, "color": NAVY, "space_before": 14}),
    ], spacing=1.25)
    textbox(s, 0.85, 5.75, 11.7, 0.6, [("只有要素抽取缺口时才兜底调模型一次", {"size": 16, "color": RGBColor(0x6B, 0x72, 0x80)})])
    add_source(s, "审查链路 58 ms（正则优先，零模型调用）")

    # ---------- P8 可信性 ----------
    s = blank(prs)
    add_title(s, "可信性：每一条结论都能点回原文")
    pic = s.shapes.add_picture(str(SHOT_REVIEW), Inches(0.55), Inches(1.45), width=Inches(8.9))
    while pic.height > Inches(4.95):
        pic.height = int(pic.height * 0.97)
        pic.width = int(pic.width * 0.97)
    pl, pt = pic.left / 914400.0, pic.top / 914400.0
    pw, ph = pic.width / 914400.0, pic.height / 914400.0

    def arrow(x1, y1, x2, y2):
        conn = s.shapes.add_connector(2, Inches(x1), Inches(y1), Inches(x2), Inches(y2))
        conn.line.color.rgb = NAVY
        conn.line.width = Pt(2)
        ln = conn.line._get_or_add_ln()
        tail = parse_xml('<a:tailEnd %s type="triangle" w="med" len="med"/>' % nsdecls("a"))
        ln.append(tail)
        return conn

    def badge(x, y, text):
        size = 0.34
        dot = s.shapes.add_shape(9, Inches(x - size / 2), Inches(y - size / 2), Inches(size), Inches(size))
        dot.fill.solid(); dot.fill.fore_color.rgb = NAVY
        dot.line.color.rgb = WHITE; dot.line.width = Pt(1.5); dot.shadow.inherit = False
        tf = dot.text_frame
        tf.margin_left = 0; tf.margin_right = 0; tf.margin_top = 0; tf.margin_bottom = 0
        p = tf.paragraphs[0]; p.alignment = PP_ALIGN.CENTER
        run = p.add_run(); run.text = text
        style_run(run, 12, True, WHITE)
        dot.text_frame.vertical_anchor = MSO_ANCHOR.MIDDLE

    marks = [
        ("1", "① 框住原文", "风险条款在 PDF 上被框出", 0.30, 0.60),
        ("2", "② 条款号", "对应合同第几条", 0.905, 0.335),
        ("3", "③ 风险说明", "改了怎么写 / 依据哪条法", 0.80, 0.665),
    ]
    for i, (num, title, desc, fx, fy) in enumerate(marks):
        mx = pl + fx * pw
        my = pt + fy * ph
        badge(mx, my, num)
        top = 1.75 + i * 1.55
        textbox(s, 9.95, top, 3.0, 0.4, [(title, {"size": 18, "bold": True, "color": NAVY})])
        textbox(s, 9.95, top + 0.38, 3.0, 0.8, [(desc, {"size": 13, "color": GRAY})])
        arrow(9.9, top + 0.3, pl + pw + 0.08, top + 0.3)
    textbox(s, 0.55, 6.6, 12.2, 0.4, [("引用文本由数据库回填，模型无权直写法条原文", {"size": 16, "bold": True, "color": NAVY})])
    add_source(s, "线上演示环境 laborlaw-ai.cn 实拍")

    # ---------- P9 工程质量 ----------
    s = blank(prs)
    add_title(s, "工程质量")
    add_rule(s)
    blocks = [
        ("19 个 Go 包", "go test ./... 全绿"),
        ("215 个前端测试", "node --test + vue-tsc"),
        ("7 次数据库迁移", "schema_migrations 到 7"),
        ("7 条运行指标", "/metrics（对外屏蔽）"),
        ("双桶限流 + 日预算", "IP 20/分 · 账号 5/分；预算写穿 Redis，重启不清零"),
        ("合同可解释", "每条 finding 带规则 ID / 条款 / 坐标"),
    ]
    for i, (t, d) in enumerate(blocks):
        col, row = i % 3, i // 3
        left = 0.85 + col * 4.0
        top = 1.7 + row * 2.3
        card(s, left, top, 3.75, 1.9, fill=LIGHT)
        textbox(s, left + 0.25, top + 0.25, 3.3, 0.5, [(t, {"size": 22, "bold": True, "color": NAVY})])
        textbox(s, left + 0.25, top + 0.85, 3.3, 0.9, [(d, {"size": 14, "color": GRAY})], spacing=1.2)
    add_source(s, "后端 19 包 / 前端 215 条 / 迁移 7 个 / 指标 7 条")

    # ---------- P10 局限 ----------
    s = blank(prs)
    add_title(s, "局限：说清楚边界")
    add_rule(s)
    textbox(s, 0.95, 1.8, 11.4, 4.2, [
        ("· 只覆盖 11 部法、21 条规则，判例与地方口径未入", {"size": 22, "color": GRAY}),
        ("· 扫描件不做 OCR，直接明确拒绝", {"size": 22, "color": GRAY, "space_before": 22}),
        ("「这份文件没有文字层，可能是扫描件，暂不支持审查」", {"size": 15, "color": RGBColor(0x6B, 0x72, 0x80), "space_before": 6}),
        ("· 多轮上下文有 token 预算上限，超长对话会裁掉早期轮次", {"size": 22, "color": GRAY, "space_before": 20}),
    ], spacing=1.4)
    add_source(s, "系统能力边界（实际提示语取自线上页面）")

    # ---------- P11 数据总结 ----------
    s = blank(prs)
    add_title(s, "数据总结")
    add_rule(s)
    rows = [
        ("检索准确率（@3 / @5）", "98.2%（110/112） / 99.1%（111/112）"),
        ("法条库", "596 条 · 11 部法律法规"),
        ("风险规则", "21 条"),
        ("合同审查耗时", "58 ms"),
        ("对话首字延迟", "0.6 s"),
    ]
    table = s.shapes.add_table(len(rows) + 1, 2, Inches(0.85), Inches(1.7), Inches(11.6), Inches(4.2)).table
    table.columns[0].width = Inches(4.4)
    table.columns[1].width = Inches(7.2)
    headers = ["指标", "数值"]
    for j, h in enumerate(headers):
        cell = table.cell(0, j)
        cell.text = ""
        p = cell.text_frame.paragraphs[0]
        run = p.add_run()
        run.text = h
        style_run(run, 18, True, WHITE)
        cell.fill.solid(); cell.fill.fore_color.rgb = NAVY
    for i, (k, v) in enumerate(rows, start=1):
        for j, text in enumerate((k, v)):
            cell = table.cell(i, j)
            cell.text = ""
            p = cell.text_frame.paragraphs[0]
            run = p.add_run()
            run.text = text
            style_run(run, 18, j == 1, NAVY if j == 1 else GRAY)
            cell.fill.solid(); cell.fill.fore_color.rgb = WHITE if i % 2 else LIGHT
            cell.vertical_anchor = MSO_ANCHOR.MIDDLE
    add_source(s, "以上数字来自评测与线上运行统计")

    # ---------- P12 演示 ----------
    s = blank(prs)
    bar = s.shapes.add_shape(1, Inches(0), Inches(0), Inches(13.333), Inches(0.18))
    bar.fill.solid(); bar.fill.fore_color.rgb = NAVY; bar.line.fill.background(); bar.shadow.inherit = False
    textbox(s, 0.9, 2.15, 11.5, 1.0, [("下面用一份真实劳动合同演示", {"size": 40, "bold": True, "color": NAVY})])
    steps = ["① 问一句", "② 传一份合同", "③ 点一张风险卡"]
    for i, st in enumerate(steps):
        left = 1.1 + i * 3.9
        card(s, left, 3.9, 3.4, 1.3, fill=LIGHT)
        textbox(s, left + 0.25, 4.25, 2.9, 0.6, [(st, {"size": 24, "bold": True, "color": NAVY})])
    add_source(s, "演示环境 laborlaw-ai.cn（演示号）")

    OUT.parent.mkdir(parents=True, exist_ok=True)
    prs.save(str(OUT))
    print("SAVED", OUT, len(prs.slides._sldIdLst), "slides")


if __name__ == "__main__":
    main()
