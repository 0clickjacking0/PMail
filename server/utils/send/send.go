package send

import (
	"errors"
	"fmt"
	"github.com/Jinnrry/pmail/config"
	"github.com/Jinnrry/pmail/dto/parsemail"
	"github.com/Jinnrry/pmail/models"
	"github.com/Jinnrry/pmail/utils/array"
	"github.com/Jinnrry/pmail/utils/async"
	"github.com/Jinnrry/pmail/utils/consts"
	"github.com/Jinnrry/pmail/utils/context"
	"github.com/Jinnrry/pmail/utils/smtp"
	log "github.com/sirupsen/logrus"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"time"
)

type mxDomain struct {
	recipientDomain string
	failureKey      string
	mxHost          string
}

type lookupMXFunc func(string) ([]*net.MX, error)
type plaintextSendFunc func(addr, from, fromDomain string, to []string, data []byte) error

type temporaryMXFallbackError struct {
	lookupErr   error
	fallbackErr error
}

func (e *temporaryMXFallbackError) Error() string {
	return fmt.Sprintf("temporary MX lookup failed (%v); fallback delivery failed: %v", e.lookupErr, e.fallbackErr)
}

func (e *temporaryMXFallbackError) Unwrap() error {
	return e.lookupErr
}

// Forward 转发邮件
func Forward(ctx *context.Context, e *parsemail.Email, forwardAddress string, user *models.User) error {

	log.WithContext(ctx).Debugf("开始转发邮件")

	b := e.ForwardBuildBytes(ctx, user, forwardAddress)

	log.WithContext(ctx).Debugf("%s", b)

	from := user.Account + "@" + config.Instance.Domains[0]
	return forwardData(ctx, config.Instance.Domains[0], b, forwardAddress, from)
}

func ForwardRaw(ctx *context.Context, e *parsemail.Email, rawEmailData []byte, forwardAddress string, user *models.User) error {
	log.WithContext(ctx).Debugf("开始原始邮件转发")

	from := user.Account + "@" + config.Instance.Domains[0]
	return forwardData(ctx, config.Instance.Domains[0], rawEmailData, forwardAddress, from)
}

func forwardData(ctx *context.Context, fromDomain string, data []byte, forwardAddress string, from string) error {
	var to []*parsemail.User
	to = []*parsemail.User{
		{EmailAddress: forwardAddress},
	}

	err, _ := doSend(ctx, fromDomain, data, to, from)
	return err
}

func Send(ctx *context.Context, e *parsemail.Email) (error, map[string]error) {

	_, fromDomain := e.From.GetDomainAccount()

	b := e.BuildBytes(ctx, true)

	var to []*parsemail.User
	to = append(append(append(to, e.To...), e.Cc...), e.Bcc...)

	return doSend(ctx, fromDomain, b, to, e.From.EmailAddress)

}

func doSend(ctx *context.Context, fromDomain string, data []byte, to []*parsemail.User, from string) (error, map[string]error) {
	return doSendWith(ctx, fromDomain, data, to, from, net.LookupMX, sendPlaintext)
}

func doSendWith(ctx *context.Context, fromDomain string, data []byte, to []*parsemail.User, from string, lookupMX lookupMXFunc, sendMail plaintextSendFunc) (error, map[string]error) {
	startedAt := time.Now()

	// 按域名整理
	toByDomain := map[mxDomain][]*parsemail.User{}
	mxLookupErrors := map[mxDomain]error{}
	// mxHostsByFailureKey 保存每个域名按优先级排序的全部 MX 主机，
	// 投递失败时逐个故障转移（RFC 5321 §5.1）
	mxHostsByFailureKey := map[string][]string{}
	for _, s := range to {
		args := strings.Split(s.EmailAddress, "@")
		if len(args) == 2 {
			if args[1] == consts.TEST_DOMAIN {
				// 测试使用
				address := mxDomain{
					recipientDomain: args[1],
					failureKey:      "localhost",
					mxHost:          "127.0.0.1",
				}
				toByDomain[address] = append(toByDomain[address], s)
			} else {
				//查询dns mx记录
				mxInfo, lookupErr := lookupMX(args[1])
				address := mxDomain{
					recipientDomain: args[1],
					failureKey:      "smtp." + args[1],
					mxHost:          "smtp." + args[1],
				}
				if lookupErr != nil {
					log.WithContext(ctx).Errorf("%s 域名mx记录查询失败，检查邮箱是否存在！", s.EmailAddress)
				}
				if len(mxInfo) > 0 {
					address = mxDomain{
						recipientDomain: args[1],
						failureKey:      args[1],
						mxHost:          mxInfo[0].Host,
					}
					// net.LookupMX 已按优先级排序，保存全部 MX 用于故障转移
					hosts := make([]string, 0, len(mxInfo))
					for _, mx := range mxInfo {
						hosts = append(hosts, mx.Host)
					}
					mxHostsByFailureKey[address.failureKey] = hosts
				}
				if lookupErr != nil {
					mxLookupErrors[address] = lookupErr
				}
				toByDomain[address] = append(toByDomain[address], s)
			}
		} else {
			log.WithContext(ctx).Errorf("邮箱地址解析错误！ %s", s)
			continue
		}
	}

	var errEmailAddress []string
	var errEmailAddressMu sync.Mutex

	errMap := sync.Map{}

	as := async.New(ctx)
	for domain, tos := range toByDomain {
		domain := domain
		tos := tos
		mxLookupErr := mxLookupErrors[domain]
		as.WaitProcess(func(p any) {
			recordFailure := func(err error) {
				err = deliveryFailureCause(mxLookupErr, err)
				log.WithContext(ctx).Errorf("%v 邮件投递失败%+v", tos, err)

				errEmailAddressMu.Lock()
				for _, user := range tos {
					errEmailAddress = append(errEmailAddress, user.EmailAddress)
				}
				errEmailAddressMu.Unlock()

				errMap.Store(domain.failureKey, err)
			}

			// 按优先级逐个尝试 MX 主机（RFC 5321 §5.1）：
			// 网络错误/超时等临时失败转移下一个；5xx 永久错误直接失败（换 MX 也不会成功）
			mxHosts := mxHostsByFailureKey[domain.failureKey]
			if len(mxHosts) == 0 {
				mxHosts = []string{domain.mxHost}
			}
			var lastErr error
			for i, mxHost := range mxHosts {
				smtpStartedAt := time.Now()
				err := sendMail(mxHost+":25", from, fromDomain, buildAddress(tos), data)
				smtpDuration := time.Since(smtpStartedAt)
				if err == nil {
					log.WithContext(ctx).Infof("Outbound SMTP delivery path=plaintext port=25 domain=%s mx=%s mx_attempt=%d/%d recipients=%d result=success smtp_duration=%s", domain.recipientDomain, mxHost, i+1, len(mxHosts), len(tos), smtpDuration)
					return
				}
				log.WithContext(ctx).Infof("Outbound SMTP delivery path=plaintext port=25 domain=%s mx=%s mx_attempt=%d/%d recipients=%d result=failure smtp_duration=%s", domain.recipientDomain, mxHost, i+1, len(mxHosts), len(tos), smtpDuration)
				lastErr = err
				if isPermanentSMTPResponse(err) {
					break
				}
			}
			recordFailure(lastErr)
		}, nil)
	}
	as.Wait()

	orgMap := map[string]error{}
	errMap.Range(func(key, value any) bool {
		if value != nil {
			orgMap[key.(string)] = value.(error)
		} else {
			orgMap[key.(string)] = nil
		}

		return true
	})

	result := "success"
	if len(errEmailAddress) > 0 {
		result = "failure"
	}
	log.WithContext(ctx).Infof("Outbound SMTP batch path=plaintext port=25 domains=%d failed_recipients=%d result=%s duration=%s", len(toByDomain), len(errEmailAddress), result, time.Since(startedAt))

	if len(errEmailAddress) > 0 {
		return errors.New("以下收件人投递失败：" + array.Join(errEmailAddress, ",")), orgMap
	}
	return nil, orgMap
}

func sendPlaintext(addr, from, fromDomain string, to []string, data []byte) error {
	return smtp.SendMailUnsafe("", addr, nil, from, fromDomain, to, data)
}

func isPermanentSMTPResponse(err error) bool {
	var protocolErr *textproto.Error
	return errors.As(err, &protocolErr) && protocolErr.Code >= 500 && protocolErr.Code <= 599
}

func deliveryFailureCause(mxLookupErr, fallbackErr error) error {
	if fallbackErr == nil || isPermanentSMTPResponse(fallbackErr) {
		return fallbackErr
	}

	var lookupDNSErr *net.DNSError
	if !errors.As(mxLookupErr, &lookupDNSErr) || (!lookupDNSErr.IsTimeout && !lookupDNSErr.IsTemporary) {
		return fallbackErr
	}

	var networkErr net.Error
	if !errors.As(fallbackErr, &networkErr) {
		return fallbackErr
	}

	return &temporaryMXFallbackError{
		lookupErr:   mxLookupErr,
		fallbackErr: fallbackErr,
	}
}

func buildAddress(u []*parsemail.User) []string {
	var ret []string

	for _, user := range u {
		ret = append(ret, user.EmailAddress)

	}

	return ret
}
