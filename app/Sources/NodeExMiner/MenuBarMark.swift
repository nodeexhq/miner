import AppKit

/// The NVTO mark: a chip with its corners cut, split into four pieces by
/// three cuts that draw an N. The four paths are copied from the site's
/// master drawing (web/public/mark.svg, a 1024 × 1024 box). The same file is
/// in the Miner and Connect apps.
public enum NodeExMark {
    static let paths = [
        "M170.7 53.3 L224 0 L653 0 L653 535.6Z",
        "M819 19 L1024 224 L1024 800 L970.7 853.3 L819 701.6Z",
        "M853.3 970.7 L800 1024 L371 1024 L371 488.4Z",
        "M205 1005 L0 800 L0 224 L53.3 170.7 L205 322.4Z"
    ]

    /// Fills the mark into `rect` in the current colour. The context must be
    /// flipped (y down), as SVG is.
    public static func fill(in rect: CGRect, context ctx: CGContext) {
        ctx.saveGState()
        ctx.translateBy(x: rect.minX, y: rect.minY)
        ctx.scaleBy(x: rect.width / 1024, y: rect.height / 1024)
        for d in paths {
            ctx.addPath(path(d))
            ctx.fillPath()
        }
        ctx.restoreGState()
    }

    /// Reads SVG path data using absolute M, L, C, H, V and Z only.
    static func path(_ d: String) -> CGPath {
        let p = CGMutablePath()
        var nums: [CGFloat] = []
        var cmd: Character = " "
        var cur = CGPoint.zero
        func flush() {
            switch cmd {
            case "M": cur = CGPoint(x: nums[0], y: nums[1]); p.move(to: cur)
            case "C":
                var i = 0
                while i + 5 < nums.count {
                    cur = CGPoint(x: nums[i + 4], y: nums[i + 5])
                    p.addCurve(to: cur, control1: CGPoint(x: nums[i], y: nums[i + 1]), control2: CGPoint(x: nums[i + 2], y: nums[i + 3]))
                    i += 6
                }
            case "L":
                var i = 0
                while i + 1 < nums.count {
                    cur = CGPoint(x: nums[i], y: nums[i + 1]); p.addLine(to: cur)
                    i += 2
                }
            case "H": cur.x = nums[0]; p.addLine(to: cur)
            case "V": cur.y = nums[0]; p.addLine(to: cur)
            case "Z": p.closeSubpath()
            default: break
            }
            nums = []
        }
        var num = ""
        for ch in d + " " {
            if ch.isLetter {
                if !num.isEmpty { nums.append(CGFloat(Double(num)!)); num = "" }
                flush(); cmd = ch
            } else if ch == " " || ch == "," || (ch == "-" && !num.isEmpty) {
                if !num.isEmpty { nums.append(CGFloat(Double(num)!)); num = "" }
                if ch == "-" { num = "-" }
            } else {
                num.append(ch)
            }
        }
        flush()
        return p
    }
}

/// The mark for the menu bar, in one colour as a template image so macOS
/// tints it for light and dark menu bars. The two apps sit side by side in
/// the menu bar, so they differ in shape: Connect shows the bare mark, the
/// Miner the mark cut out of a solid tile (this Mac is a node).
public enum MenuBarMark {
    public enum Badge { case none, dot, dimmed }
    public enum Style { case plain, tile }

    public static func image(style: Style = .plain, badge: Badge = .none, height: CGFloat = 16) -> NSImage {
        let size = NSSize(width: height, height: height)
        let img = NSImage(size: size, flipped: true) { _ in
            guard let ctx = NSGraphicsContext.current?.cgContext else { return false }
            let alpha: CGFloat = badge == .dimmed ? 0.45 : 1
            ctx.setFillColor(NSColor.black.withAlphaComponent(alpha).cgColor)
            let box = CGRect(origin: .zero, size: size)
            switch style {
            case .plain:
                NodeExMark.fill(in: box, context: ctx)
            case .tile:
                let tile = box.insetBy(dx: height * 0.03, dy: height * 0.03)
                ctx.addPath(CGPath(roundedRect: tile, cornerWidth: height * 0.26, cornerHeight: height * 0.26, transform: nil))
                ctx.fillPath()
                ctx.setBlendMode(.clear)
                NodeExMark.fill(in: box.insetBy(dx: height * 0.17, dy: height * 0.17), context: ctx)
                ctx.setBlendMode(.normal)
            }
            if badge == .dot {
                // A connection or a lease: a solid dot on the cut top-right
                // corner, with a clear ring so it reads against the chip.
                let s = height / 1024
                ctx.setBlendMode(.clear)
                ctx.fillEllipse(in: CGRect(x: (930 - 150) * s, y: (150 - 150) * s, width: 300 * s, height: 300 * s))
                ctx.setBlendMode(.normal)
                ctx.setFillColor(NSColor.black.cgColor)
                ctx.fillEllipse(in: CGRect(x: (930 - 100) * s, y: (150 - 100) * s, width: 200 * s, height: 200 * s))
            }
            return true
        }
        img.isTemplate = true
        return img
    }
}
