import Foundation
import CoreGraphics
import ImageIO
import UniformTypeIdentifiers

// Re-encode a PNG with no alpha channel: App Store Connect rejects app icons
// that carry one, even when every pixel is fully opaque.
let args = CommandLine.arguments
guard args.count == 3,
      let src = CGImageSourceCreateWithURL(URL(fileURLWithPath: args[1]) as CFURL, nil),
      let img = CGImageSourceCreateImageAtIndex(src, 0, nil) else {
    FileHandle.standardError.write("usage: flatten <in.png> <out.png>\n".data(using: .utf8)!)
    exit(1)
}
let w = img.width, h = img.height
guard let ctx = CGContext(data: nil, width: w, height: h, bitsPerComponent: 8,
                          bytesPerRow: 0, space: CGColorSpaceCreateDeviceRGB(),
                          bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue) else { exit(1) }
// Paint white first so any translucent source pixel composites onto a known
// ground rather than picking up uninitialized memory.
ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))
ctx.fill(CGRect(x: 0, y: 0, width: w, height: h))
ctx.draw(img, in: CGRect(x: 0, y: 0, width: w, height: h))
guard let out = ctx.makeImage(),
      let dest = CGImageDestinationCreateWithURL(URL(fileURLWithPath: args[2]) as CFURL,
                                                 UTType.png.identifier as CFString, 1, nil) else { exit(1) }
CGImageDestinationAddImage(dest, out, nil)
exit(CGImageDestinationFinalize(dest) ? 0 : 1)
