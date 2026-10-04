#!/usr/bin/env python3
"""Capture source protobuf/HTTP encoding and JWT tests without changing the legacy checkout.

This harness records transport behavior, not database-backed application responses.
It must be supplemented by the isolated legacy application acceptance capture.
"""
from pathlib import Path
import os
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT.parent / 'go-tangra-sms-gw'
OUT = ROOT / 'tests/fixtures/legacy'
GO = os.environ.get('GO', 'go')
MAIN = r'''
package main

import (
 "crypto/hmac"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "net/http/httptest"
 "os"
 "path/filepath"
 kratos "github.com/go-kratos/kratos/v2/transport/http"
 pb "github.com/go-tangra/go-tangra-sms-gw/gen/go/sms_gw/service/v1"
 "google.golang.org/protobuf/types/known/emptypb"
)

func main() {
 out := os.Args[1]
 expires := int64(7200)
 values := map[string]any{
  "login": &pb.LoginResponse{AccessToken:"fixture-access", RefreshToken:"fixture-refresh", TokenType:"Bearer",ExpiresIn:&expires},
  "logout": &emptypb.Empty{},
  "me": &pb.GetMeResponse{User:&pb.ApiClientPublic{Id:7,Username:"fixture_client",Authority:"API_CLIENT"},Abilities:[]*pb.Ability{{Action:"create",Subject:"sms"},{Action:"list",Subject:"sms"},{Action:"get",Subject:"sms"},{Action:"get",Subject:"dlr"}}},
  "send": &pb.SendSmsResponse{Data:&pb.Message{Id:"00000000-0000-0000-0000-000000000001",Owner:7,To:359888123456,Status:4294967295,Text:"Hello fixture",ProviderId:3},EndpointResponse:&pb.VoicecomResponse{ReturnCode:0,ReturnMessage:"OK",Channels:&pb.Channels{Sms:&pb.SmsChannel{SendOrder:11,MessageParts:1}}}},
  "get": &pb.GetSmsMessageResponse{Data:&pb.Message{Id:"00000000-0000-0000-0000-000000000001",To:18446744073709551615,Status:1}},
  "list-empty": &pb.ListSmsResponse{},
  "dlrs": &pb.ListDlrResponse{Items:[]*pb.Dlr{{Id:1,RequestId:"00000000-0000-0000-0000-000000000001",MessageStatus:1,To:359888123456,RemoteAddrerss:"192.0.2.1",PartsReceived:2}},Total:1},
  "send-request": &pb.SendSmsRequest{To:359888123456,ProviderId:3,TemplateId:4,Properties:map[string]string{"Name":"fixture"},Sms:&pb.Sms{From:"Fixture",Encoding:"gsm-03-38",Concatenate:1}},
  "paging-defaults": &pb.PagingRequest{},
 }
 for name,v := range values {
  w:=httptest.NewRecorder(); req:=httptest.NewRequest("GET","http://fixture/",nil)
  if err:=kratos.DefaultResponseEncoder(w,req,v);err!=nil {panic(err)}
  save(out,name,w)
 }
 errors:=map[string]error{
  "missing-token":pb.ErrorUnauthorized("missing bearer token"),
  "invalid-token":pb.ErrorUnauthorized("invalid token"),
  "expired-token":pb.ErrorUnauthorized("token expired"),
  "wrong-kind":pb.ErrorUnauthorized("wrong token kind"),
  "bad-request":pb.ErrorBadRequest("username and password are required"),
  "invalid-credentials":pb.ErrorUnauthorized("invalid credentials"),
  "disabled-account":pb.ErrorUnauthorized("account disabled"),
  "forbidden":pb.ErrorForbidden("fixture"),
  "not-found":pb.ErrorRecordNotFound("fixture"),
  "conflict":pb.ErrorRecordAlreadyExists("fixture"),
  "invalid-parameter":pb.ErrorInvalidParameter("fixture"),
  "rate-limit":pb.ErrorTooManyRequests("fixture"),
  "internal":pb.ErrorInternalError("fixture"),
  "carrier-error":pb.ErrorThirdPartyServiceInternalError("fixture"),
  "carrier-code":pb.ErrorThirdPartyServiceInvalidCode("fixture"),
  "database":pb.ErrorDbUnavailable("fixture"),
 }
 for name,e:=range errors {
  w:=httptest.NewRecorder();kratos.DefaultErrorEncoder(w,httptest.NewRequest("GET","http://fixture/",nil),e);save(out,"error-"+name,w)
 }
 w:=httptest.NewRecorder();req:=httptest.NewRequest("GET","http://fixture/dlr",nil)
 if err:=kratos.DefaultResponseEncoder(w,req,"DLR_OK");err!=nil{panic(err)};save(out,"dlr-ack",w)
 // The payload struct below is copied field-for-field from source dispatcher.go;
 // stable field order matters because the signature covers exact JSON bytes.
 payload:=struct{
  MessageID string `json:"message_id"`;Channel string `json:"channel"`;MessageStatus uint32 `json:"message_status"`;StatusText string `json:"status_text"`;To uint64 `json:"to"`;From string `json:"from"`;Timestamp uint64 `json:"timestamp"`
 }{"00000000-0000-0000-0000-000000000001","sms",1,"Delivered",359888123456,"Fixture",1700000000}
 body,err:=json.Marshal(payload);if err!=nil{panic(err)}
 mac:=hmac.New(sha256.New,[]byte("fixture-secret-not-a-production-key"));mac.Write([]byte("1700000000."));mac.Write(body)
 data,err:=json.MarshalIndent(map[string]any{"body":string(body),"timestamp":"1700000000","signature":hex.EncodeToString(mac.Sum(nil)),"secret":"fixture-secret-not-a-production-key"},"","  ");if err!=nil{panic(err)}
 if err:=os.WriteFile(filepath.Join(out,"callback.json"),append(data,'\n'),0644);err!=nil{panic(err)}
 fmt.Printf("Captured %d source codec/error cases and signed callback bytes\n",len(values)+len(errors)+1)
}
func save(out,name string,w *httptest.ResponseRecorder){
 data,err:=json.MarshalIndent(map[string]any{"status":w.Code,"content_type":w.Header().Get("Content-Type"),"body":w.Body.String()},"","  ");if err!=nil{panic(err)}
 if err:=os.WriteFile(filepath.Join(out,name+".json"),append(data,'\n'),0644);err!=nil{panic(err)}
}
'''


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='smsgw-legacy-capture-') as temp:
        tmp = Path(temp)
        for name in ['go.mod', 'go.sum']:
            shutil.copyfile(SOURCE / name, tmp / name)
        shutil.copytree(SOURCE / 'gen/go', tmp / 'gen/go')
        auth = tmp / 'internal/auth'
        auth.mkdir(parents=True)
        for name in ['jwt.go', 'jwt_test.go', 'password.go', 'password_test.go', 'denylist.go', 'errors.go']:
            shutil.copyfile(SOURCE / 'internal/auth' / name, auth / name)
        (tmp / 'main.go').write_text(MAIN)
        env = dict(os.environ, GOCACHE=os.environ.get('GOCACHE', '/tmp/smsgw-go-build'))
        # A selected Go binary discovers its own stdlib; an inherited GVM
        # GOROOT may point at a different compiler and break compilation.
        if os.environ.get('GO'):
            env.pop('GOROOT', None)
        subprocess.run([GO, 'run', '-mod=mod', '.', str(OUT)], cwd=tmp, env=env, check=True)
        subprocess.run([GO, 'test', '-race', './internal/auth'], cwd=tmp, env=env, check=True)


if __name__ == '__main__':
    main()
