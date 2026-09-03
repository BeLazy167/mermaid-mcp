import assert from "node:assert/strict";
import dgram from "node:dgram";
import net from "node:net";

function expectDenied(socket) {
  return new Promise((resolve, reject) => {
    socket.once("error", (error) => {
      try {
        assert.ok(["EACCES", "EPERM"].includes(error.code), `unexpected error ${error.code}`);
        resolve();
      } catch (assertion) {
        reject(assertion);
      }
    });
    socket.once("connect", () => reject(new Error("network connection unexpectedly succeeded")));
    socket.once("listening", () => reject(new Error("network bind unexpectedly succeeded")));
  });
}

const tcp = net.connect({ host: "1.1.1.1", port: 443 });
const udp = dgram.createSocket("udp4");
const denied = Promise.all([expectDenied(tcp), expectDenied(udp)]);
udp.bind(0);
await denied;
tcp.destroy();
udp.close();
