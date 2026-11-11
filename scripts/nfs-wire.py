"""Minimal cache-free NFSv3 client for the private daemon stress harness."""
import socket
import struct


def u32(n):
    return struct.pack('!I', n)


def opaque(data):
    return u32(len(data)) + data + b'\0' * (-len(data) % 4)


class Reader:
    def __init__(self, data):
        self.data, self.pos = data, 0

    def word(self):
        n = struct.unpack_from('!I', self.data, self.pos)[0]
        self.pos += 4
        return n

    def opaque(self):
        n = self.word()
        data = self.data[self.pos:self.pos+n]
        self.pos += (n + 3) & ~3
        return data


class NFSWire:
    def __init__(self, port):
        self.port, self.xid = port, 0

    def call(self, program, proc, data):
        self.xid += 1
        request = struct.pack('!10I', self.xid, 0, 2, program, 3, proc, 0, 0, 0, 0) + data
        with socket.create_connection(('127.0.0.1', self.port), timeout=20) as sock:
            sock.sendall(u32(0x80000000 | len(request)) + request)
            def exact(n):
                result = b''
                while len(result) < n:
                    part = sock.recv(n - len(result))
                    if not part:
                        raise RuntimeError('short RPC reply')
                    result += part
                return result
            reply = b''
            while True:
                mark = struct.unpack('!I', exact(4))[0]
                reply += exact(mark & 0x7fffffff)
                if mark & 0x80000000:
                    break
        r = Reader(reply)
        assert (r.word(), r.word(), r.word()) == (self.xid, 1, 0)
        r.word()  # verifier flavor
        r.opaque()
        assert r.word() == 0, 'RPC rejected'
        status = r.word()
        if status:
            raise RuntimeError(f'NFS procedure {proc}: status {status}')
        return r

    def mount(self):
        return self.call(100005, 1, opaque(b'/')).opaque()

    def lookup(self, directory, name):
        return self.call(100003, 3, opaque(directory) + opaque(name.encode())).opaque()

    def getattr(self, handle):
        r = self.call(100003, 1, opaque(handle))
        return r.data[r.pos:]

    def read(self, handle):
        r = self.call(100003, 6, opaque(handle) + struct.pack('!QI', 0, 4096))
        if r.word():
            r.pos += 84  # fattr3
        count, eof = r.word(), r.word()
        data = r.opaque()
        assert count == len(data)
        return data
