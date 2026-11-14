import AppKit

/// The NodeEx mark for the menu bar: the sphere and its orbit, drawn in one
/// colour as a template image so macOS tints it for light and dark menu
/// bars. It is the same drawing in the Miner and Connect apps.
public enum MenuBarMark {
    public enum Badge { case none, dot, dimmed }

    public static func image(badge: Badge = .none, height: CGFloat = 16) -> NSImage {
        // The artwork's own units: a 64 × 44 box, as in the site's mark.svg.
        let scale = height / 44
        let size = NSSize(width: 64 * scale, height: height)
        let img = NSImage(size: size, flipped: true) { _ in
            guard let ctx = NSGraphicsContext.current?.cgContext else { return false }
            ctx.scaleBy(x: scale, y: scale)
            let alpha: CGFloat = badge == .dimmed ? 0.45 : 1
            NSColor.black.withAlphaComponent(alpha).set()
            let line: CGFloat = 3.2
            let orbit = { (front: Bool) -> NSBezierPath in
                // The orbit is an ellipse of 28 × 9.5 around the centre,
                // tilted 24°; the front half passes over the sphere.
                let p = NSBezierPath()
                p.appendArc(withCenter: .zero, radius: 1, startAngle: front ? 0 : 180, endAngle: front ? 180 : 360)
                var t = AffineTransform(translationByX: 32, byY: 22)
                t.rotate(byDegrees: 24)
                t.scale(x: 28, y: 9.5)
                p.transform(using: t)
                p.lineWidth = line
                p.lineCapStyle = .round
                return p
            }
            orbit(false).stroke()
            NSBezierPath(ovalIn: NSRect(x: 32 - 16.6, y: 22 - 16.6, width: 33.2, height: 33.2)).fill()
            // The moon on the orbit, in the sphere's own frame.
            var moon = AffineTransform(translationByX: 32, byY: 22)
            moon.rotate(byDegrees: 24)
            let m = moon.transform(NSPoint(x: 19.7, y: 6.7))
            // Cut a gap round the front arc and the moon so they read
            // against the sphere, then draw them.
            ctx.setBlendMode(.clear)
            let gap = orbit(true); gap.lineWidth = line + 3.6; gap.stroke()
            NSBezierPath(ovalIn: NSRect(x: m.x - 5.6, y: m.y - 5.6, width: 11.2, height: 11.2)).fill()
            ctx.setBlendMode(.normal)
            orbit(true).stroke()
            NSBezierPath(ovalIn: NSRect(x: m.x - 3.6, y: m.y - 3.6, width: 7.2, height: 7.2)).fill()
            if badge == .dot {
                // A connection or a lease: a solid dot at the top right.
                NSColor.black.set()
                ctx.setBlendMode(.clear)
                NSBezierPath(ovalIn: NSRect(x: 50, y: -1, width: 15, height: 15)).fill()
                ctx.setBlendMode(.normal)
                NSBezierPath(ovalIn: NSRect(x: 53, y: 2, width: 9, height: 9)).fill()
            }
            return true
        }
        img.isTemplate = true
        return img
    }
}
