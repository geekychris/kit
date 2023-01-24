package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/alexec/kit/internal/proc"

	"github.com/alexec/kit/internal/types"
	"github.com/fatih/color"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"
	k8sstrings "k8s.io/utils/strings"
	"sigs.k8s.io/yaml"
)

//go:generate sh -c "git describe --tags > tag"
//go:embed tag
var tag string

func init() {
	_ = os.Mkdir("logs", 0o777)
	f, err := os.OpenFile("logs/kit.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		panic(err)
	}
	log.SetOutput(f)
	log.Println(tag)
}

const escape = "\x1b"

const defaultConfigFile = "tasks.yaml"

func main() {
	cmd := &cobra.Command{
		Use:          "kit [TASK_NAME...]",
		Short:        "Run tasks",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			configFile := defaultConfigFile

			ctx, stopEverything := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stopEverything()

			_ = os.Mkdir("logs", 0777)

			pod := &types.Pod{}

			in, err := os.ReadFile(configFile)
			if err != nil {
				return err
			}
			if err = yaml.UnmarshalStrict(in, pod); err != nil {
				return err
			}
			data, err := yaml.Marshal(pod)
			if err != nil {
				return err
			}
			if err = os.WriteFile(configFile, data, 0o0644); err != nil {
				return err
			}

			tasks := pod.Spec.Tasks.NeededFor(args)

			statuses := &types.Status{}
			logEntries := make(map[string]*types.LogEntry)

			for _, task := range tasks {
				logEntries[task.Name] = &types.LogEntry{}
				statuses.TaskStatuses = append(statuses.TaskStatuses, &types.TaskStatus{
					Name: task.Name,
					State: types.TaskState{
						Waiting: &types.TaskStateWaiting{Reason: "waiting"},
					},
				})
			}

			// every 1/2 second print the current status to the terminal
			go func() {
				defer handleCrash(stopEverything)
				for {
					width, _, _ := terminal.GetSize(0)
					if width == 0 {
						width = 80
					}
					fmt.Printf("%s[2J", escape)   // clear screen
					fmt.Printf("%s[0;0H", escape) // move to 0,0
					for _, t := range pod.Spec.Tasks {
						state := statuses.GetStatus(t.Name)
						if state == nil {
							continue
						}
						if state.IsSuccess() {
							continue
						}
						reason := state.GetReason()
						icon := "▓"
						switch reason {
						case "running":
							icon = color.BlueString("▓")
						case "ready":
							icon = color.GreenString("▓")
						case "error":
							icon = color.RedString("▓")
						}
						prefix := fmt.Sprintf("%s %-10s %-8s", icon, k8sstrings.ShortenString(state.Name, 10), reason)
						if ports := t.GetHostPorts(); len(ports) > 0 {
							prefix = prefix + " " + color.HiBlackString(fmt.Sprint(ports))
						}
						entry := logEntries[t.Name]
						msgWidth := width - len(prefix) - 1
						msg := ""
						if msgWidth > 0 {
							msg = k8sstrings.ShortenString(entry.Msg, msgWidth)
							if entry.IsError() {
								msg = color.YellowString(msg)
							}
						}
						fmt.Println(prefix + " " + msg)
					}
					time.Sleep(time.Second / 2)
				}
			}()

			work := make(chan types.Task)

			go func() {
				defer handleCrash(stopEverything)
				for _, t := range tasks.GetLeaves() {
					work <- t
				}
			}()

			go func() {
				defer handleCrash(stopEverything)
				<-ctx.Done()
				close(work)
			}()

			wg := sync.WaitGroup{}

			stop := &sync.Map{}

			maybeStartDownstream := func(name string) {
				for _, downstream := range tasks.GetDownstream(name) {
					ok := true
					for _, upstream := range downstream.Dependencies {
						ok = ok && statuses.GetStatus(upstream).IsFulfilled()
					}
					if ok {
						work <- downstream
					}
				}
			}

			// stop everthing if all tasks are complete
			go func() {
				defer handleCrash(stopEverything)
				for {
					complete := true
					for _, task := range tasks {
						complete = complete && !task.IsBackground() && statuses.GetStatus(task.Name).IsTerminated()
					}
					if complete {
						stopEverything()
					}
					time.Sleep(time.Second)
				}
			}()

			for t := range work {
				name := t.Name

				logEntry := logEntries[name]

				prc := proc.New(t, pod.Spec)

				processCtx, stopProcess := context.WithCancel(ctx)
				defer stopProcess()

				// watch files for changes
				go func(t types.Task, stopProcess func()) {
					defer handleCrash(stopEverything)
					watcher, err := fsnotify.NewWatcher()
					if err != nil {
						panic(err)
					}
					defer watcher.Close()
					for _, w := range t.Watch {
						stat, err := os.Stat(w)
						if err != nil {
							panic(err)
						}
						if err := watcher.Add(w); err != nil {
							panic(err)
						}
						if stat.IsDir() {
							if err := filepath.WalkDir(w, func(path string, d fs.DirEntry, err error) error {
								if err != nil {
									return err
								}
								if d.IsDir() {
									logEntry.Printf("%q watching %q\n", t.Name, path)
									return watcher.Add(path)
								}
								return nil
							}); err != nil {
								panic(err)
							}
						}
					}

					timer := time.AfterFunc(100*365*24*time.Hour, func() {
						work <- t
					})
					defer timer.Stop()

					for {
						select {
						case <-processCtx.Done():
							return
						case e := <-watcher.Events:
							logEntry.Printf("%v changed\n", e)
							timer.Reset(time.Second)
						case err := <-watcher.Errors:
							panic(err)
						}
					}
				}(t, stopProcess)

				// run the process
				wg.Add(1)
				go func(t types.Task, status *types.TaskStatus, stopProcess func()) {
					defer handleCrash(stopEverything)
					defer wg.Done()

					if f, ok := stop.Load(name); ok {
						logEntry.Printf("stopping process")
						f.(func())()
					}

					stop.Store(name, stopProcess)

					mutex := proc.KeyLock("/main/proc/" + name)
					logEntry.Printf("waiting for mutex\n")
					mutex.Lock()
					logEntry.Printf("locked mutex\n")
					defer mutex.Unlock()

					logFile, err := os.Create(filepath.Join("logs", name+".log"))
					if err != nil {
						panic(err)
					}
					defer logFile.Close()
					stdout := io.MultiWriter(logFile, logEntry.Stdout())
					stderr := io.MultiWriter(logFile, logEntry.Stderr())
					for {
						select {
						case <-processCtx.Done():
							return
						default:
							err := func() error {
								for _, port := range t.GetHostPorts() {
									if err := isPortOpen(port); err != nil {
										return err
									}
								}
								return nil
							}()
							if err != nil {
								status.State = types.TaskState{
									Terminated: &types.TaskStateTerminated{Reason: "error"},
								}
								_, _ = fmt.Fprintln(stderr, err.Error())
							} else {
								logEntry.Printf("starting process")
								err := func() error {
									runCtx, stopRun := context.WithCancel(processCtx)
									defer stopRun()
									go func() {
										defer handleCrash(stopEverything)
										<-ctx.Done()
										stopProcess()
									}()
									status.Ready = false
									status.State = types.TaskState{
										Running: &types.TaskStateRunning{},
									}
									if probe := t.GetLivenessProbe(); probe != nil {
										liveFunc := func(live bool, err error) {
											if !live {
												_, _ = fmt.Fprintf(stderr, "liveness live=%v err=%v\n", live, err)
												stopRun()
											} else {
												_, _ = fmt.Fprintf(stdout, "liveness live=%v\n", live)
											}
										}
										go probeLoop(runCtx, stopEverything, *probe, liveFunc)
									}
									if probe := t.GetReadinessProbe(); probe != nil {
										readyFunc := func(ready bool, err error) {
											status.Ready = ready
											if ready {
												_, _ = fmt.Fprintf(stdout, "readiness ready=%v\n", status.Ready)
												maybeStartDownstream(name)
											} else {
												_, _ = fmt.Fprintf(stderr, "readiness ready=%v err=%v\n", ready, err)
											}
										}
										go probeLoop(runCtx, stopEverything, *probe, readyFunc)
									}
									return prc.Run(runCtx, stdout, stderr)
								}()
								if err != nil {
									if errors.Is(err, context.Canceled) {
										return
									}
									status.State = types.TaskState{
										Terminated: &types.TaskStateTerminated{Reason: "error"},
									}
									_, _ = fmt.Fprintln(stderr, err.Error())
								} else {
									status.State = types.TaskState{
										Terminated: &types.TaskStateTerminated{Reason: "success"},
									}
									maybeStartDownstream(name)
									if !t.IsBackground() {
										return
									}
								}
							}
							time.Sleep(2 * time.Second)
						}
					}
				}(t, statuses.GetStatus(t.Name), stopProcess)

				time.Sleep(time.Second / 4)
			}

			wg.Wait()

			for _, task := range tasks {
				if statuses.GetStatus(task.Name).Failed() {
					return fmt.Errorf("%s failed", task.Name)
				}
			}

			return nil
		},
	}

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func handleCrash(stop func()) {
	if r := recover(); r != nil {
		fmt.Println(r)
		debug.PrintStack()
		stop()
	}
}
