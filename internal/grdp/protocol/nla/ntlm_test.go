package nla_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"

	"github.com/lunixbochs/struc"
	"github.com/stubbedev/win-rdp-mcp/internal/grdp/core"
	"github.com/stubbedev/win-rdp-mcp/internal/grdp/protocol/nla"
)

func TestNewNegotiateMessage(t *testing.T) {
	ntlm := nla.NewNTLMv2("", "", "")
	negoMsg := ntlm.GetNegotiateMessage()
	buff := &bytes.Buffer{}
	struc.Pack(buff, negoMsg)

	result := hex.EncodeToString(buff.Bytes())
	expected := "4e544c4d53535000010000003582086000000000000000000000000000000000"

	if result != expected {
		t.Error(result, " not equals to", expected)
	}
}

func TestNTLMv2_ComputeResponse(t *testing.T) {
	ntlm := nla.NewNTLMv2("", "", "")

	ResponseKeyNT, _ := hex.DecodeString("39e32c766260586a9036f1ceb04c3007")
	ResponseKeyLM, _ := hex.DecodeString("39e32c766260586a9036f1ceb04c3007")
	ServerChallenge, _ := hex.DecodeString("adcb9d1c8d4a5ed8")
	ClienChallenge, _ := hex.DecodeString("1a78bed8e5d5efa7")
	Timestamp, _ := hex.DecodeString("a02f44f01267d501")
	ServerName, _ := hex.DecodeString("02001e00570049004e002d00460037005200410041004d004100500034004a00430001001e00570049004e002d00460037005200410041004d004100500034004a00430004001e00570049004e002d00460037005200410041004d004100500034004a00430003001e00570049004e002d00460037005200410041004d004100500034004a00430007000800a02f44f01267d50100000000")

	NtChallengeResponse, LmChallengeResponse, SessionBaseKey := ntlm.ComputeResponseV2(ResponseKeyNT, ResponseKeyLM, ServerChallenge, ClienChallenge, Timestamp, ServerName)

	ntChallRespExpected := "4e7316531937d2fc91e7230853844b890101000000000000a02f44f01267d5011a78bed8e5d5efa70000000002001e00570049004e002d00460037005200410041004d004100500034004a00430001001e00570049004e002d00460037005200410041004d004100500034004a00430004001e00570049004e002d00460037005200410041004d004100500034004a00430003001e00570049004e002d00460037005200410041004d004100500034004a00430007000800a02f44f01267d50100000000"
	lmChallRespExpected := "d4dc6edc0c37dd70f69b5c4f05a615661a78bed8e5d5efa7"
	sessBaseKeyExpected := "034009be89a0507b2bd6d28e966e1dab"

	if hex.EncodeToString(NtChallengeResponse) != ntChallRespExpected {
		t.Error("NtChallengeResponse incorrect")
	}

	if hex.EncodeToString(LmChallengeResponse) != lmChallRespExpected {
		t.Error("LmChallengeResponse incorrect")
	}

	if hex.EncodeToString(SessionBaseKey) != sessBaseKeyExpected {
		t.Error("SessionBaseKey incorrect")
	}
}

const challengeHeaderLen = 56

func avPairs(pairs ...[]byte) []byte {
	var b []byte
	for _, p := range pairs {
		b = append(b, p...)
	}
	return append(b, 0, 0, 0, 0)
}

func avPair(id uint16, value []byte) []byte {
	p := make([]byte, 4, 4+len(value))
	binary.LittleEndian.PutUint16(p, id)
	binary.LittleEndian.PutUint16(p[2:], uint16(len(value)))
	return append(p, value...)
}

func challengeMessage(targetName, targetInfo []byte) []byte {
	b := make([]byte, challengeHeaderLen, challengeHeaderLen+len(targetName)+len(targetInfo))
	copy(b, "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(b[8:], 2)
	binary.LittleEndian.PutUint16(b[12:], uint16(len(targetName)))
	binary.LittleEndian.PutUint16(b[14:], uint16(len(targetName)))
	binary.LittleEndian.PutUint32(b[16:], challengeHeaderLen)
	binary.LittleEndian.PutUint32(b[20:], nla.NTLMSSP_NEGOTIATE_VERSION|nla.NTLMSSP_NEGOTIATE_UNICODE|
		nla.NTLMSSP_NEGOTIATE_KEY_EXCH|nla.NTLMSSP_NEGOTIATE_128|nla.NTLMSSP_NEGOTIATE_SEAL|
		nla.NTLMSSP_NEGOTIATE_SIGN|nla.NTLMSSP_NEGOTIATE_TARGET_INFO|nla.NTLMSSP_NEGOTIATE_EXTENDED_SESSIONSECURITY)
	copy(b[24:32], "srvchall")
	binary.LittleEndian.PutUint16(b[40:], uint16(len(targetInfo)))
	binary.LittleEndian.PutUint16(b[42:], uint16(len(targetInfo)))
	binary.LittleEndian.PutUint32(b[44:], uint32(challengeHeaderLen+len(targetName)))
	b[48], b[49], b[55] = 10, 0, nla.NTLMSSP_REVISION_W2K3
	b = append(b, targetName...)
	return append(b, targetInfo...)
}

func validChallenge() []byte {
	name := core.UnicodeEncode("TARGET")
	info := avPairs(avPair(nla.MsvAvNbComputerName, name), avPair(nla.MsvAvTimestamp, []byte{1, 2, 3, 4, 5, 6, 7, 8}))
	return challengeMessage(name, info)
}

func TestAuthenticateDoesNotLogSessionKeys(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ntlm := nla.NewNTLMv2("DOM", "user", "Tr0ub4dor&3")
	ntlm.GetNegotiateMessage()
	auth, sec := ntlm.GetAuthenticateMessage(validChallenge())
	if auth == nil || sec == nil {
		t.Fatal("a well-formed challenge was rejected")
	}
	out := logs.String()
	for _, key := range [][]byte{sec.SigningKey, sec.VerifyKey} {
		if strings.Contains(out, hex.EncodeToString(key)) {
			t.Errorf("an NTLM session key reached the log:\n%s", out)
		}
	}
	if strings.Contains(out, "Tr0ub4dor&3") {
		t.Errorf("the password reached the log:\n%s", out)
	}
}
