// Minimal NEP-141-compatible test contract for exercising the NEAR wallet's
// TokenWallet code paths (ft_balance_of, storage_balance_of, storage_deposit,
// ft_transfer) against a real NEAR runtime. Not a spec-complete NEP-141/145
// implementation -- just enough wire-protocol compatibility to validate the
// Go client. Includes a test-only `mint` method with no real-world analog.

const std = @import("std");

extern "env" fn input(register_id: u64) void;
extern "env" fn register_len(register_id: u64) u64;
extern "env" fn read_register(register_id: u64, ptr: u64) void;
extern "env" fn value_return(len: u64, ptr: u64) void;
extern "env" fn predecessor_account_id(register_id: u64) void;
extern "env" fn attached_deposit(ptr: u64) void;
extern "env" fn storage_write(key_len: u64, key_ptr: u64, value_len: u64, value_ptr: u64, register_id: u64) u64;
extern "env" fn storage_read(key_len: u64, key_ptr: u64, register_id: u64) u64;
extern "env" fn storage_has_key(key_len: u64, key_ptr: u64) u64;
extern "env" fn panic_utf8(len: u64, ptr: u64) noreturn;

const REG: u64 = 0;

// Manual byte copy, avoiding @memcpy: Zig's wasm32 codegen lowers @memcpy to
// the bulk-memory-proposal `memory.copy` instruction regardless of -mcpu
// feature flags, which NEAR's wasm preparer rejects (CompilationError:
// PrepareError: Deserialization) since it only accepts core WASM MVP
// opcodes.
fn copyBytes(dst: []u8, src: []const u8) void {
    var i: usize = 0;
    while (i < src.len) : (i += 1) {
        dst[i] = src[i];
    }
}

var input_buf: [512]u8 = undefined;
var scratch_buf: [512]u8 = undefined;
var key_buf: [128]u8 = undefined;
var out_buf: [24]u8 = undefined;
var acct_buf: [64]u8 = undefined;
var pred_buf: [64]u8 = undefined;

fn fail(msg: []const u8) noreturn {
    panic_utf8(msg.len, @intFromPtr(msg.ptr));
}

fn readInput() []u8 {
    input(REG);
    const len: usize = @intCast(register_len(REG));
    read_register(REG, @intFromPtr(&input_buf[0]));
    return input_buf[0..len];
}

fn readPredecessor() []u8 {
    predecessor_account_id(REG);
    const len: usize = @intCast(register_len(REG));
    read_register(REG, @intFromPtr(&pred_buf[0]));
    return pred_buf[0..len];
}

// Finds `"key":"` in json and returns the substring up to the closing quote,
// copied into buf. Panics if the field isn't present.
fn extractField(json: []const u8, key: []const u8, buf: []u8) []u8 {
    var needle_buf: [40]u8 = undefined;
    var n: usize = 0;
    needle_buf[n] = '"';
    n += 1;
    for (key) |c| {
        needle_buf[n] = c;
        n += 1;
    }
    needle_buf[n] = '"';
    n += 1;
    needle_buf[n] = ':';
    n += 1;
    needle_buf[n] = '"';
    n += 1;
    const needle = needle_buf[0..n];

    var i: usize = 0;
    while (i + needle.len <= json.len) : (i += 1) {
        if (std.mem.eql(u8, json[i .. i + needle.len], needle)) {
            const start = i + needle.len;
            var j = start;
            while (j < json.len and json[j] != '"') : (j += 1) {}
            const val = json[start..j];
            copyBytes(buf[0..val.len], val);
            return buf[0..val.len];
        }
    }
    fail("field not found");
}

fn parseU64(s: []const u8) u64 {
    var v: u64 = 0;
    for (s) |c| {
        v = v * 10 + (c - '0');
    }
    return v;
}

fn writeU64Decimal(v: u64, buf: []u8) []u8 {
    if (v == 0) {
        buf[0] = '0';
        return buf[0..1];
    }
    var tmp: [20]u8 = undefined;
    var n: usize = 0;
    var x = v;
    while (x > 0) : (x /= 10) {
        tmp[n] = @as(u8, @intCast(x % 10)) + '0';
        n += 1;
    }
    var i: usize = 0;
    while (i < n) : (i += 1) {
        buf[i] = tmp[n - 1 - i];
    }
    return buf[0..n];
}

fn balanceKey(account: []const u8, buf: []u8) []u8 {
    buf[0] = 'b';
    buf[1] = ':';
    copyBytes(buf[2 .. 2 + account.len], account);
    return buf[0 .. 2 + account.len];
}

fn storageKey(account: []const u8, buf: []u8) []u8 {
    buf[0] = 's';
    buf[1] = ':';
    copyBytes(buf[2 .. 2 + account.len], account);
    return buf[0 .. 2 + account.len];
}

fn getBalance(account: []const u8) u64 {
    const key = balanceKey(account, &key_buf);
    const found = storage_read(key.len, @intFromPtr(key.ptr), REG);
    if (found == 0) return 0;
    const len: usize = @intCast(register_len(REG));
    read_register(REG, @intFromPtr(&scratch_buf[0]));
    return parseU64(scratch_buf[0..len]);
}

fn setBalance(account: []const u8, v: u64) void {
    const key = balanceKey(account, &key_buf);
    const val = writeU64Decimal(v, &out_buf);
    _ = storage_write(key.len, @intFromPtr(key.ptr), val.len, @intFromPtr(val.ptr), REG);
}

fn markRegistered(account: []const u8) void {
    const key = storageKey(account, &key_buf);
    const one = "1";
    _ = storage_write(key.len, @intFromPtr(key.ptr), one.len, @intFromPtr(one.ptr), REG);
}

fn isRegistered(account: []const u8) bool {
    const key = storageKey(account, &key_buf);
    return storage_has_key(key.len, @intFromPtr(key.ptr)) != 0;
}

fn depositIsOneYocto() bool {
    var dep: [16]u8 = undefined;
    attached_deposit(@intFromPtr(&dep[0]));
    if (dep[0] != 1) return false;
    for (dep[1..16]) |b| {
        if (b != 0) return false;
    }
    return true;
}

export fn ft_balance_of() void {
    const args = readInput();
    const account = extractField(args, "account_id", &acct_buf);
    const bal = getBalance(account);
    var quoted: [26]u8 = undefined;
    quoted[0] = '"';
    const digits = writeU64Decimal(bal, quoted[1..]);
    quoted[1 + digits.len] = '"';
    value_return(2 + digits.len, @intFromPtr(&quoted[0]));
}

export fn storage_balance_of() void {
    const args = readInput();
    const account = extractField(args, "account_id", &acct_buf);
    if (isRegistered(account)) {
        const o = "{}";
        value_return(o.len, @intFromPtr(o.ptr));
    } else {
        const n = "null";
        value_return(n.len, @intFromPtr(n.ptr));
    }
}

export fn storage_deposit() void {
    const args = readInput();
    const account = extractField(args, "account_id", &acct_buf);
    markRegistered(account);
    const o = "{}";
    value_return(o.len, @intFromPtr(o.ptr));
}

export fn ft_transfer() void {
    const args = readInput();
    var receiver_buf: [64]u8 = undefined;
    var amount_buf: [24]u8 = undefined;
    const receiver = extractField(args, "receiver_id", &receiver_buf);
    const amount_s = extractField(args, "amount", &amount_buf);
    const amount = parseU64(amount_s);

    if (!depositIsOneYocto()) fail("ft_transfer requires exactly 1 yoctoNEAR attached");

    const sender = readPredecessor();

    const sender_bal = getBalance(sender);
    if (sender_bal < amount) fail("the account doesn't have enough balance");
    if (!isRegistered(receiver)) fail("receiver not registered");

    setBalance(sender, sender_bal - amount);
    const receiver_bal = getBalance(receiver);
    setBalance(receiver, receiver_bal + amount);
}

// Test-only: mints `amount` to `account_id`, registering it if needed. Not
// part of NEP-141; used solely to seed a starting balance in the harness.
export fn mint() void {
    const args = readInput();
    var acct2_buf: [64]u8 = undefined;
    var amt_buf: [24]u8 = undefined;
    const account = extractField(args, "account_id", &acct2_buf);
    const amount_s = extractField(args, "amount", &amt_buf);
    const amount = parseU64(amount_s);

    markRegistered(account);
    const bal = getBalance(account);
    setBalance(account, bal + amount);
}
