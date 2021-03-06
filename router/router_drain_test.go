package router_test

import (
	"io/ioutil"
	"net/http"
	"os"
	"syscall"
	"time"

	"errors"

	"github.com/cloudfoundry/gorouter/access_log"
	vcap "github.com/cloudfoundry/gorouter/common"
	cfg "github.com/cloudfoundry/gorouter/config"
	"github.com/cloudfoundry/gorouter/metrics/fakes"
	"github.com/cloudfoundry/gorouter/proxy"
	rregistry "github.com/cloudfoundry/gorouter/registry"
	"github.com/cloudfoundry/gorouter/route"
	. "github.com/cloudfoundry/gorouter/router"
	"github.com/cloudfoundry/gorouter/test"
	"github.com/cloudfoundry/gorouter/test_util"
	vvarz "github.com/cloudfoundry/gorouter/varz"
	"github.com/cloudfoundry/gunk/natsrunner"
	"github.com/cloudfoundry/yagnats"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Router", func() {
	var (
		natsRunner *natsrunner.NATSRunner
		config     *cfg.Config

		mbusClient yagnats.NATSConn
		registry   *rregistry.RouteRegistry
		varz       vvarz.Varz
		router     *Router
		natsPort   uint16
	)

	testAndVerifyRouterStopsNoDrain := func(signals chan os.Signal, closeChannel chan struct{}, sigs ...os.Signal) {
		app := test.NewTestApp([]route.Uri{"drain.vcap.me"}, config.Port, mbusClient, nil, "")
		blocker := make(chan bool)
		resultCh := make(chan bool, 2)
		app.AddHandler("/", func(w http.ResponseWriter, r *http.Request) {
			blocker <- true

			_, err := ioutil.ReadAll(r.Body)
			defer r.Body.Close()
			Expect(err).ToNot(HaveOccurred())

			<-blocker

			w.WriteHeader(http.StatusNoContent)
		})

		app.Listen()

		Eventually(func() bool {
			return appRegistered(registry, app)
		}).Should(BeTrue())

		go func() {
			defer GinkgoRecover()
			req, err := http.NewRequest("GET", app.Endpoint(), nil)
			Expect(err).ToNot(HaveOccurred())

			client := http.Client{}
			resp, err := client.Do(req)
			Expect(err).ToNot(HaveOccurred())
			Expect(resp).ToNot(BeNil())
			Expect(resp.StatusCode).ToNot(Equal(http.StatusNoContent))
			defer resp.Body.Close()
			resultCh <- false
		}()

		<-blocker

		go func() {
			for _, s := range sigs {
				signals <- s
			}
		}()

		Eventually(closeChannel).Should(BeClosed())

		var result bool
		Eventually(resultCh).Should(Receive(&result))
		Expect(result).To(BeFalse())

		blocker <- false
	}

	runRouter := func(r *Router) (chan os.Signal, chan struct{}) {
		signals := make(chan os.Signal)
		readyChan := make(chan struct{})
		closeChannel := make(chan struct{})
		go func() {
			r.Run(signals, readyChan)
			close(closeChannel)
		}()
		select {
		case <-readyChan:
		}
		return signals, closeChannel
	}

	BeforeEach(func() {
		natsPort = test_util.NextAvailPort()
		natsRunner = natsrunner.NewNATSRunner(int(natsPort))
		natsRunner.Start()

		proxyPort := test_util.NextAvailPort()
		statusPort := test_util.NextAvailPort()

		config = test_util.SpecConfig(natsPort, statusPort, proxyPort)
		config.EndpointTimeout = 5 * time.Second

		mbusClient = natsRunner.MessageBus
		registry = rregistry.NewRouteRegistry(config, mbusClient, new(fakes.FakeRouteReporter))
		varz = vvarz.NewVarz(registry)
		logcounter := vcap.NewLogCounter()
		proxy := proxy.NewProxy(proxy.ProxyArgs{
			EndpointTimeout: config.EndpointTimeout,
			Ip:              config.Ip,
			TraceKey:        config.TraceKey,
			Registry:        registry,
			Reporter:        varz,
			AccessLogger:    &access_log.NullAccessLogger{},
		})

		errChan := make(chan error, 2)
		var err error
		router, err = NewRouter(config, proxy, mbusClient, registry, varz, logcounter, errChan)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		if natsRunner != nil {
			natsRunner.Stop()
		}
	})

	Context("Drain", func() {
		BeforeEach(func() {
			runRouter(router)
		})

		AfterEach(func() {
			if router != nil {
				router.Stop()
			}
		})

		It("waits until the last request completes", func() {
			app := test.NewTestApp([]route.Uri{"drain.vcap.me"}, config.Port, mbusClient, nil, "")
			blocker := make(chan bool)
			resultCh := make(chan bool, 2)
			app.AddHandler("/", func(w http.ResponseWriter, r *http.Request) {
				blocker <- true

				_, err := ioutil.ReadAll(r.Body)
				defer r.Body.Close()
				Expect(err).ToNot(HaveOccurred())

				<-blocker

				w.WriteHeader(http.StatusNoContent)
			})

			app.Listen()

			Eventually(func() bool {
				return appRegistered(registry, app)
			}).Should(BeTrue())

			drainTimeout := 1 * time.Second

			go func() {
				defer GinkgoRecover()
				req, err := http.NewRequest("GET", app.Endpoint(), nil)
				Expect(err).ToNot(HaveOccurred())

				client := http.Client{}
				resp, err := client.Do(req)
				Expect(err).ToNot(HaveOccurred())
				Expect(resp).ToNot(BeNil())
				defer resp.Body.Close()
				_, err = ioutil.ReadAll(resp.Body)
				Expect(err).ToNot(HaveOccurred())
				resultCh <- false
			}()

			<-blocker
			go func() {
				defer GinkgoRecover()
				err := router.Drain(drainTimeout)
				Expect(err).ToNot(HaveOccurred())
				resultCh <- true
			}()

			Consistently(resultCh, drainTimeout/10).ShouldNot(Receive())

			blocker <- false

			var result bool
			Eventually(resultCh).Should(Receive(&result))
			Expect(result).To(BeTrue())
		})

		It("times out if it takes too long", func() {
			app := test.NewTestApp([]route.Uri{"draintimeout.vcap.me"}, config.Port, mbusClient, nil, "")

			blocker := make(chan bool)
			resultCh := make(chan error, 2)
			app.AddHandler("/", func(w http.ResponseWriter, r *http.Request) {
				blocker <- true

				_, err := ioutil.ReadAll(r.Body)
				defer r.Body.Close()
				Expect(err).ToNot(HaveOccurred())

				time.Sleep(1 * time.Second)
			})
			app.Listen()

			Eventually(func() bool {
				return appRegistered(registry, app)
			}).Should(BeTrue())

			go func() {
				defer GinkgoRecover()
				req, err := http.NewRequest("GET", app.Endpoint(), nil)
				Expect(err).ToNot(HaveOccurred())

				client := http.Client{}
				resp, err := client.Do(req)
				Expect(err).ToNot(HaveOccurred())
				Expect(resp).ToNot(BeNil())
				defer resp.Body.Close()
			}()

			<-blocker

			go func() {
				defer GinkgoRecover()
				err := router.Drain(500 * time.Millisecond)
				resultCh <- err
			}()

			var result error
			Eventually(resultCh).Should(Receive(&result))
			Expect(result).To(Equal(DrainTimeout))
		})
	})

	Context("OnErrOrSignal", func() {
		Context("when an error is received in the error channel", func() {
			var errChan chan error

			BeforeEach(func() {
				logcounter := vcap.NewLogCounter()
				proxy := proxy.NewProxy(proxy.ProxyArgs{
					EndpointTimeout: config.EndpointTimeout,
					Ip:              config.Ip,
					TraceKey:        config.TraceKey,
					Registry:        registry,
					Reporter:        varz,
					AccessLogger:    &access_log.NullAccessLogger{},
				})

				errChan = make(chan error, 2)
				var err error
				router, err = NewRouter(config, proxy, mbusClient, registry, varz, logcounter, errChan)
				Expect(err).ToNot(HaveOccurred())
				runRouter(router)
			})

			It("it drains existing connections and stops the router", func() {
				app := test.NewTestApp([]route.Uri{"drain.vcap.me"}, config.Port, mbusClient, nil, "")
				blocker := make(chan bool)
				resultCh := make(chan bool, 2)
				app.AddHandler("/", func(w http.ResponseWriter, r *http.Request) {
					blocker <- true

					_, err := ioutil.ReadAll(r.Body)
					defer r.Body.Close()
					Expect(err).ToNot(HaveOccurred())

					<-blocker

					w.WriteHeader(http.StatusNoContent)
				})

				app.Listen()

				Eventually(func() bool {
					return appRegistered(registry, app)
				}).Should(BeTrue())

				drainTimeout := 1 * time.Second

				go func() {
					defer GinkgoRecover()
					req, err := http.NewRequest("GET", app.Endpoint(), nil)
					Expect(err).ToNot(HaveOccurred())

					client := http.Client{}
					resp, err := client.Do(req)
					Expect(err).ToNot(HaveOccurred())
					Expect(resp).ToNot(BeNil())
					defer resp.Body.Close()
					_, err = ioutil.ReadAll(resp.Body)
					Expect(err).ToNot(HaveOccurred())
					resultCh <- false
				}()

				<-blocker

				go func() {
					errChan <- errors.New("Fake error")
				}()

				Consistently(resultCh, drainTimeout/10).ShouldNot(Receive())

				blocker <- false

				var result bool
				Eventually(resultCh).Should(Receive(&result))
				Expect(result).To(BeFalse())

				req, err := http.NewRequest("GET", app.Endpoint(), nil)
				Expect(err).ToNot(HaveOccurred())

				client := http.Client{}
				_, err = client.Do(req)
				Expect(err).To(HaveOccurred())
			})
		})

		Context("when a USR1 signal is sent", func() {
			var (
				signals chan os.Signal
			)

			BeforeEach(func() {
				signals, _ = runRouter(router)
			})

			It("it drains and stops the router", func() {
				app := test.NewTestApp([]route.Uri{"drain.vcap.me"}, config.Port, mbusClient, nil, "")
				blocker := make(chan bool)
				resultCh := make(chan bool, 2)
				app.AddHandler("/", func(w http.ResponseWriter, r *http.Request) {
					blocker <- true

					_, err := ioutil.ReadAll(r.Body)
					defer r.Body.Close()
					Expect(err).ToNot(HaveOccurred())

					<-blocker

					w.WriteHeader(http.StatusNoContent)
				})

				app.Listen()

				Eventually(func() bool {
					return appRegistered(registry, app)
				}).Should(BeTrue())

				drainTimeout := 1 * time.Second

				go func() {
					defer GinkgoRecover()
					req, err := http.NewRequest("GET", app.Endpoint(), nil)
					Expect(err).ToNot(HaveOccurred())

					client := http.Client{}
					resp, err := client.Do(req)
					Expect(err).ToNot(HaveOccurred())
					Expect(resp).ToNot(BeNil())
					defer resp.Body.Close()
					_, err = ioutil.ReadAll(resp.Body)
					Expect(err).ToNot(HaveOccurred())
					resultCh <- false
				}()

				<-blocker

				go func() {
					signals <- syscall.SIGUSR1
				}()

				Consistently(resultCh, drainTimeout/10).ShouldNot(Receive())

				blocker <- false

				var result bool
				Eventually(resultCh).Should(Receive(&result))
				Expect(result).To(BeFalse())
			})
		})

		Context("when a SIGTERM signal is sent", func() {
			It("it drains and stops the router", func() {
				signals, closeChannel := runRouter(router)
				testAndVerifyRouterStopsNoDrain(signals, closeChannel, syscall.SIGTERM)
			})
		})

		Context("when a SIGINT signal is sent", func() {
			It("it drains and stops the router", func() {
				signals, closeChannel := runRouter(router)
				testAndVerifyRouterStopsNoDrain(signals, closeChannel, syscall.SIGINT)
			})
		})

		Context("when USR1 is the first of multiple signals sent", func() {
			It("it drains and stops the router", func() {
				signals, _ := runRouter(router)
				app := test.NewTestApp([]route.Uri{"drain.vcap.me"}, config.Port, mbusClient, nil, "")
				blocker := make(chan bool)
				resultCh := make(chan bool, 2)
				app.AddHandler("/", func(w http.ResponseWriter, r *http.Request) {
					blocker <- true

					_, err := ioutil.ReadAll(r.Body)
					defer r.Body.Close()
					Expect(err).ToNot(HaveOccurred())

					<-blocker

					w.WriteHeader(http.StatusNoContent)
				})

				app.Listen()

				Eventually(func() bool {
					return appRegistered(registry, app)
				}).Should(BeTrue())

				drainTimeout := 1 * time.Second

				go func() {
					defer GinkgoRecover()
					req, err := http.NewRequest("GET", app.Endpoint(), nil)
					Expect(err).ToNot(HaveOccurred())

					client := http.Client{}
					resp, err := client.Do(req)
					Expect(err).ToNot(HaveOccurred())
					Expect(resp).ToNot(BeNil())
					defer resp.Body.Close()
					_, err = ioutil.ReadAll(resp.Body)
					Expect(err).ToNot(HaveOccurred())
					resultCh <- false
				}()

				<-blocker

				go func() {
					signals <- syscall.SIGUSR1
					signals <- syscall.SIGTERM
				}()

				Consistently(resultCh, drainTimeout/10).ShouldNot(Receive())

				blocker <- false

				var result bool
				Eventually(resultCh).Should(Receive(&result))
				Expect(result).To(BeFalse())
			})
		})

		Context("when USR1 is not the first of multiple signals sent", func() {
			It("it does not drain and stops the router", func() {
				signals, closeChannel := runRouter(router)
				testAndVerifyRouterStopsNoDrain(signals, closeChannel, syscall.SIGINT, syscall.SIGUSR1)
			})
		})

		Context("when a non handlded signal is sent", func() {
			It("it drains and stops the router", func() {
				signals, closeChannel := runRouter(router)
				testAndVerifyRouterStopsNoDrain(signals, closeChannel, syscall.SIGUSR2)
			})
		})
	})

})
