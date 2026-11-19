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
	enrollFile  string
)

var enrollCommand = cobra.Command{
	Use:   "enroll",
	Short: "用面板生成的一次性接入码接入 Gvboard 面板（节点机模式）",
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
		if err := machine.Save(enrollFile, cred); err != nil {
			return fmt.Errorf("保存凭据失败: %w", err)
		}
		fmt.Printf("接入成功（节点机 #%d），凭据已保存到 %s，重启 v2node 后生效\n", cred.MachineID, enrollFile)
		return nil
	},
}

func init() {
	enrollCommand.Flags().StringVar(&enrollPanel, "panel", "", "面板地址，如 https://panel.example.com")
	enrollCommand.Flags().StringVar(&enrollCode, "code", "", "面板生成的一次性接入码")
	enrollCommand.Flags().StringVar(&enrollFile, "file", machine.DefaultPath, "凭据保存位置")
	command.AddCommand(&enrollCommand)
}
