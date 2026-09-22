#import <Foundation/Foundation.h>
#import <AppKit/AppKit.h>

// 通过 NSWorkspace 设置 URL scheme 默认 handler（LSSetDefaultHandlerForURLScheme 已废弃）。
void zcasSetDefaultURLHandler(const char* scheme, const char* bundlePath) {
    @autoreleasepool {
        NSString *s = [NSString stringWithUTF8String:scheme];
        NSURL *url = [NSURL fileURLWithPath:[NSString stringWithUTF8String:bundlePath]];
        [[NSWorkspace sharedWorkspace] setDefaultApplicationAtURL:url
                                              toOpenURLsWithScheme:s
                                                 completionHandler:^(NSError *error){}];
    }
}
