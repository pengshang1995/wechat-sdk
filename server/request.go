package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/pengshang1995/wechat-sdk/message"
	"github.com/pengshang1995/wechat-sdk/pay"
	"github.com/pengshang1995/wechat-sdk/util"
	"github.com/siddontang/go/log"
	"io/ioutil"
	"sort"
	"strconv"
	"strings"
)

type chooseModel struct {
	ReturnCode string `xml:"return_code"`
	ReturnMsg  string `xml:"return_msg"`
	AppID      string `xml:"appid"`
	MchID      string `xml:"mch_id"`
}

// IsWechatPay 是否是微信支付
func (m *chooseModel) IsPay() bool {
	if m.ReturnCode != "" && m.MchID != "" {
		return true
	}
	return false
}

// IsMessage 是否是常规消息体
func (m *chooseModel) IsMessage() bool {
	// 当前非支付类型的都是消息
	return !m.IsPay()
}

// HandleRequest 处理微信的请求
func (srv *Server) handleRequest() (reply *message.Reply, err error) {
	srv.requestRaw, err = ioutil.ReadAll(srv.Request.Body)
	if err != nil {
		err = fmt.Errorf("从body中解析xml失败, err=%v", err)
		return
	}
	choose := chooseModel{}
	err = xml.Unmarshal(srv.requestRaw, &choose)
	if err != nil {
		err = fmt.Errorf("无法识别响应数据, data=%s, err=%v", srv.requestRaw, err)
		return
	}
	if choose.IsPay() {
		reply, err = srv.getPay()
	} else {
		reply, err = srv.getMessage()
	}
	return
}

// handleRequestDouYin 处理抖音的请求
func (srv *Server) handleRequestDouYin() (reply *message.Reply, err error) {
	srv.requestRaw, err = ioutil.ReadAll(srv.Request.Body)
	fmt.Println(srv.requestRaw)
	if err != nil {
		err = fmt.Errorf("从body中解析xml失败, err=%v", err)
		return
	}

	reply, err = srv.getDouYinMessage()
	return
}

// getPay 解析支付消息结构
func (srv *Server) getPay() (reply *message.Reply, err error) {
	err = xml.Unmarshal(srv.requestRaw, &srv.requestPayMsg)
	if err != nil {
		return
	}
	// 解析结果非正确的，直接跳出
	if srv.requestPayMsg.ReturnCode != "SUCCESS" {
		log.Info(srv.requestRaw)
		return
	}
	// 含有加密数据
	if srv.requestPayMsg.ReqInfo != "" {
		var rawXMLMsg, encryptData []byte
		key2 := util.MD5(srv.PayKey)
		encryptData, err = base64.StdEncoding.DecodeString(srv.requestPayMsg.ReqInfo)
		if err != nil {
			if srv.debug {
				log.Warn("返回数据无法识别", srv.requestPayMsg)
			}
			return
		}
		rawXMLMsg, err = util.ECBDecrypt(encryptData, []byte(key2))
		if err != nil || len(rawXMLMsg) == 0 {
			if srv.debug {
				log.Warn(srv.random, rawXMLMsg, err)
			}
			return
		}
		err = xml.Unmarshal(rawXMLMsg, &srv.requestPayMsg)
		if err != nil {
			return
		}

	} else if !pay.VerifySign(srv.PayKey, srv.requestPayMsg) {
		log.Warn("验签失败", srv.PayKey, srv.requestPayMsg)
		return
	}
	// 判断支付返回类型
	if srv.requestPayMsg.RefundFee > 0 {
		srv.requestPayMsg.PayNotifyInfo = pay.PayTypeRefund
	} else if srv.requestPayMsg.TotalFee > 0 {
		srv.requestPayMsg.PayNotifyInfo = pay.PayTypePay
	}
	reply = srv.payHandler(srv.requestPayMsg)
	return
}

func (srv *Server) getDouYinMessage() (reply *message.Reply, err error) {
	var douYinEncryptData message.DouYinEncryptData
	err = json.Unmarshal(srv.requestRaw, &douYinEncryptData)
	if err != nil {
		err = fmt.Errorf("解析抖音验签参数失败:%s", err.Error())
		return
	}
	fmt.Println("byte callback encrypt param", douYinEncryptData)
	//验证签名
	err = VerifyByteDanceServer(srv.Token, douYinEncryptData.TimeStamp, douYinEncryptData.Nonce, douYinEncryptData.Encrypt, douYinEncryptData.MsgSignature)
	if err != nil {
		err = fmt.Errorf(err.Error())
		return
	}

	srv.requestMsgDouYin, err = DecryptByteDanceMsg(srv.EncodingAESKey, douYinEncryptData.Encrypt)
	fmt.Println("byte callback param", srv.requestMsgDouYin)

	if err != nil {
		err = fmt.Errorf(err.Error())
		return
	}
	//由于返回的没有appid 需要保存下来传入的appid
	srv.requestMsgDouYin.AppID = srv.AppID
	//
	reply = srv.douYinMessageHandler(srv.requestMsgDouYin)

	return

}

// DecryptByteDanceMsg 抖音解密
func DecryptByteDanceMsg(encodeAesKey string, encryptMsg string) (douYinMixMessage message.DouYinMixMessage, err error) {
	// get aes key
	AESKey, _ := base64.StdEncoding.DecodeString(encodeAesKey + "=")

	// decrypt msg
	decryptMsg, _ := DecryptByteDance(encryptMsg, string(AESKey))

	// plain text
	plainText := []byte(decryptMsg)
	buf := bytes.NewBuffer(plainText[16:20])
	var length int32
	_ = binary.Read(buf, binary.BigEndian, &length)

	// 推送的第三方 AppID
	appIDStart := 20 + length
	tpAppId := string(plainText[appIDStart:])
	fmt.Printf("thirdparty appid: %s\n", tpAppId)

	// 获取正常的消息体
	msgBody := string(plainText[20 : 20+length])
	fmt.Printf("decode msg body: %s\n", msgBody)

	// 返回解析的消息
	err = json.Unmarshal([]byte(msgBody), &douYinMixMessage)
	fmt.Printf("msg %+v", douYinMixMessage)
	if err != nil {
		err = fmt.Errorf("解析抖音参数失败")
	}
	return
}

func DecryptByteDance(rawData, key string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(rawData)
	if err != nil {
		return "", err
	}
	dnData, err := AESCBCDecrypt(data, []byte(key))
	if err != nil {
		return "", err
	}
	return string(dnData), nil
}

func AESCBCDecrypt(encryptData, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return []byte{}, err
	}

	blockSize := block.BlockSize()
	if len(encryptData) < blockSize {
		return []byte{}, fmt.Errorf("cipherText too short")
	}

	iv := encryptData[:blockSize]
	encryptData = encryptData[blockSize:]
	if len(encryptData)%blockSize != 0 {
		return []byte{}, fmt.Errorf("cipherText is not a multiple of the block size")
	}

	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(encryptData, encryptData)
	encryptData = PKCS7UnPadding(encryptData)

	return encryptData, nil
}

func PKCS7UnPadding(origData []byte) []byte {
	length := len(origData)
	unPadding := int(origData[length-1])
	return origData[:(length - unPadding)]
}

// VerifyByteDanceServer 抖音验签
func VerifyByteDanceServer(tpToken string, timestamp string, nonce string, encrypt string, msgSignature string) (err error) {
	values := []string{tpToken, timestamp, nonce, encrypt}
	sort.Strings(values)
	newMsgSignature := Sha1(strings.Join(values, ""))

	if newMsgSignature != msgSignature {
		err = fmt.Errorf("抖音验签失败")
	}
	return
}

func Sha1(str string) string {
	h := sha1.New()
	h.Write([]byte(str))
	encodeStr := fmt.Sprintf("%x", h.Sum(nil))
	return encodeStr
}

// getMessage 解析微信常规消息结构
func (srv *Server) getMessage() (reply *message.Reply, err error) {
	// 接收OpenId
	srv.openID = srv.Query("openid")
	// 检测数据是否加密
	srv.isSafeMode = srv.Query("encrypt_type") == "aes"
	// 检测数据签名
	if !srv.debug && srv.Query("signature") == util.Signature(srv.Token, srv.Query("timestamp"), srv.Query("nonce")) {
		err = fmt.Errorf("请求校验失败")
		return
	}
	if srv.isSafeMode {
		var encryptedXMLMsg message.EncryptedXMLMsg
		err = xml.Unmarshal(srv.requestRaw, &encryptedXMLMsg)
		if err != nil {
			err = fmt.Errorf("从body中解析xml失败,err=%v", err)
			return
		}
		//验证消息签名
		timestamp := srv.Query("timestamp")
		srv.timestamp, err = strconv.ParseInt(timestamp, 10, 32)
		if err != nil {
			return
		}
		nonce := srv.Query("nonce")
		srv.nonce = nonce
		msgSignature := srv.Query("msg_signature")
		msgSignatureGen := util.Signature(srv.Token, timestamp, nonce, encryptedXMLMsg.EncryptedMsg)
		if msgSignature != msgSignatureGen {
			err = fmt.Errorf("消息不合法，验证签名失败")
			return
		}
		//解密
		srv.random, srv.requestRaw, err = util.DecryptMsg(srv.AppID, encryptedXMLMsg.EncryptedMsg, srv.EncodingAESKey)
		if err != nil {
			err = fmt.Errorf("消息解密失败, err=%v", err)
			return
		}
	}
	err = xml.Unmarshal(srv.requestRaw, &srv.requestMsg)
	reply = srv.messageHandler(srv.requestMsg)
	return
}
