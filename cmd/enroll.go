package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/wyx2685/v2node/machine"
)

var (
	enrollPanel string
	enrollCode  string
	enrollDir   string
)

var enrollCommand = cobra.Command{
	Use:   "enroll",
	Short: "用面板生成的一次性接入码接入 Gvboard 面板（节点机模式，可接入多个面板）",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if enrollPanel == "" || enrollCode == "" {
			return fmt.Errorf("必须提供 --panel 与 --code")
		}
		host, _ := os.Hostname()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cred, err := machine.Enroll(ctx, enrollPanel, enrollCode, host, version)
		if err != nil {
			return err
		}
		path, replaced, err := machine.Add(enrollDir, machine.DefaultPath, cred)
		if err != nil {
			return fmt.Errorf("保存凭据失败: %w", err)
		}
		action := "新增"
		if replaced {
			action = "替换了该面板原有的凭据"
		}
		list, _ := machine.LoadAll(enrollDir, machine.DefaultPath)
		fmt.Printf("接入成功（面板 %s，节点机 #%d），%s，凭据已保存到 %s；本机共接入 %d 个面板，重启 v2node 后生效\n",
			cred.Panel, cred.MachineID, action, path, len(list))
		return nil
	},
}

var (
	unenrollPanel string
	unenrollDir   string
)

var unenrollCommand = cobra.Command{
	Use:   "unenroll",
	Short: "移除某个面板的节点机凭据；不带 --panel 时列出已接入的面板",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if unenrollPanel == "" {
			list, errs := machine.LoadAll(unenrollDir, machine.DefaultPath)
			for _, err := range errs {
				fmt.Println("无法读取:", err)
			}
			if len(list) == 0 {
				fmt.Println("本机没有接入任何面板")
				return nil
			}
			for _, s := range list {
				at := "-"
				if s.Cred.EnrolledAt > 0 {
					at = time.Unix(s.Cred.EnrolledAt, 0).Format("2006-01-02 15:04")
				}
				fmt.Printf("%s  节点机 #%d  接入时间 %s  %s\n", s.Cred.Panel, s.Cred.MachineID, at, s.Path)
			}
			return nil
		}
		n, err := machine.Remove(unenrollDir, machine.DefaultPath, unenrollPanel)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("本机没有接入面板 %s（不带 --panel 执行可列出已接入的面板）", unenrollPanel)
		}
		fmt.Println("已移除该面板的凭据，重启 v2node 后生效；面板上的这台节点机请在面板中删除")
		return nil
	},
}

func init() {
	enrollCommand.Flags().StringVar(&enrollPanel, "panel", "", "面板地址，如 https://panel.example.com")
	enrollCommand.Flags().StringVar(&enrollCode, "code", "", "面板生成的一次性接入码")
	enrollCommand.Flags().StringVar(&enrollDir, "dir", machine.DefaultDir, "凭据目录")
	unenrollCommand.Flags().StringVar(&unenrollPanel, "panel", "", "要移除的面板地址")
	unenrollCommand.Flags().StringVar(&unenrollDir, "dir", machine.DefaultDir, "凭据目录")
	command.AddCommand(&enrollCommand)
	command.AddCommand(&unenrollCommand)
}
