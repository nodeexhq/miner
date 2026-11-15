import AppKit

/// The NodeEx mark: a node at the centre and four arcs sweeping out to the
/// machines it connects. The eight paths are copied from the site's master
/// drawing (web/public/mark.svg, a 1024 × 1024 box); each is filled on its
/// own, because they overlap. The same file is in the Miner and Connect apps.
public enum NodeExMark {
    static let paths = [
        "M170.667 512C170.667 421.473 206.628 334.653 270.641 270.641C334.653 206.628 421.473 170.667 512 170.667V341.333C466.736 341.333 423.327 359.314 391.32 391.32C359.314 423.327 341.333 466.736 341.333 512H170.667Z",
        "M170.667 1024C170.667 933.473 206.629 846.653 270.641 782.641C334.654 718.629 421.473 682.667 512 682.667V853.333C466.737 853.333 423.327 871.314 391.321 903.32C359.315 935.327 341.334 978.736 341.334 1024H170.667Z",
        "M853.333 512C853.333 421.473 817.372 334.653 753.359 270.641C689.347 206.628 602.527 170.667 512 170.667V341.333C557.264 341.333 600.673 359.314 632.68 391.32C664.686 423.327 682.667 466.736 682.667 512H853.333Z",
        "M341.333 512C341.333 421.473 305.372 334.653 241.359 270.641C177.347 206.628 90.5272 170.667 0 170.667V341.333C45.2636 341.333 88.6734 359.314 120.68 391.32C152.686 423.327 170.667 466.736 170.667 512H341.333Z",
        "M170.667 512C170.667 602.527 206.628 689.347 270.641 753.359C334.653 817.372 421.473 853.333 512 853.333V682.667C466.737 682.667 423.327 664.686 391.32 632.68C359.314 600.673 341.333 557.264 341.333 512H170.667Z",
        "M682.667 512C682.667 602.527 718.629 689.347 782.641 753.359C846.653 817.372 933.473 853.333 1024 853.333V682.667C978.736 682.667 935.327 664.686 903.32 632.68C871.314 600.673 853.333 557.264 853.333 512H682.667Z",
        "M853.333 512C853.333 602.527 817.372 689.347 753.359 753.359C689.347 817.372 602.528 853.333 512 853.333V682.667C557.264 682.667 600.673 664.686 632.68 632.68C664.686 600.673 682.667 557.264 682.667 512H853.333Z",
        "M853.333 0C853.333 90.5272 817.372 177.347 753.359 241.359C689.347 305.372 602.527 341.333 512 341.333V170.667C557.264 170.667 600.673 152.686 632.68 120.68C664.686 88.6734 682.667 45.2636 682.667 0H853.333Z"
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

    /// Reads SVG path data using absolute M, C, H, V and Z only.
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
/// tints it for light and dark menu bars.
public enum MenuBarMark {
    public enum Badge { case none, dot, dimmed }

    public static func image(badge: Badge = .none, height: CGFloat = 16) -> NSImage {
        let size = NSSize(width: height, height: height)
        let img = NSImage(size: size, flipped: true) { _ in
            guard let ctx = NSGraphicsContext.current?.cgContext else { return false }
            let alpha: CGFloat = badge == .dimmed ? 0.45 : 1
            ctx.setFillColor(NSColor.black.withAlphaComponent(alpha).cgColor)
            NodeExMark.fill(in: CGRect(origin: .zero, size: size), context: ctx)
            if badge == .dot {
                // A connection or a lease: a solid dot in the empty top-right
                // corner, with a clear ring so it reads against the arcs.
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
