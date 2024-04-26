# 02-23

ls /root/bentoml/bentos
    我们的项目用到了yolov5和deep sort两个模型。我们没有将yolov5和deep sort模型保存到bentoml model store里面，而是直接以自定义runner到方式继承bentoml.Runnable，在内部调用了yolov5和deep sort。这种方式使用bentoml serve启动是可以使用。但是push到yatai上无法部署成功，原因是yatai server上没有项目的model。
我们不知道如果将项目打包成bentoml的模型格式？
或者，bentoml只能独立保存单个模型，不能保存包含多个模型的项目？

    the company's project is using yolov5 and deep sort models. we did not store these two model in bentoml model store, but call them by self-defining a runner which inherit the bentoml.Runnable. it is ok to start by calling the command of bentoml serve. but cannot push and deploy in yatai for there is no mode stored and pushed.

    Could anyone inform how to store these models in the format of bentoml ? or bentoml can only work on single stored model and not on multi-model project?
# 02-22

    will separateModel be the solution?

    how does the runner use model image in deployment?

    error checking push permissions -- make sure you entered the correct tag name, and that you are authenticated correctly, and try again: checking push permission for "quay.io/yatai-bentos:yatai.doc_classifier.6zhvoagqskfhrtvf": POST https://quay.io/v2/yatai-bentos/blobs/uploads/: unexpected status code 404 Not Found: 

    error checking push permissions -- make sure you entered the correct tag name, and that you are authenticated correctly, and try again: checking push permission for "quay.io/rivertown1986:yatai.doc_classifier.6zhvoagqskfhrtvf": POST https://quay.io/v2/rivertown1986/blobs/uploads/: unexpected status code 404 Not Found: 
    
    error checking push permissions -- make sure you entered the correct tag name, and that you are authenticated correctly, and try again: checking push permission for "quay.io/yatai-bentos:yatai.doc_classifier.6zhvoagqskfhrtvf": POST https://quay.io/v2/yatai-bentos/blobs/uploads/: unexpected status code 404 Not Found: 

# 02-21
    what happened when submitting a deployment?

​	trigger bentoRequest and bento CR to applied in k8s?
​
    How to create a model in the model list?

    ​	bentoml model push ?

    what happens when bentoml push ; how to attach a model:tag

    dependencies to image; copy it to client environment

# 02-02

    link between reflector and informer?
        Informer imbed controller which imbed reflector; informer.Run will call controller.Run which will contruct the reflector by the controller config and run

    ingressNginxClass -> ingressNginxService -> ingressNginxControlPod --watch-> ingress -> update nginx
    role clusterRole roleBind ClusterRoleBind ServiceAccount ==== RBAC
    ingressNginxControlPod have the AC of ingress-nginx which has a clusterRoleBind to a clusterRole that can operate on other namespace . so it can watch the ingress of other namespace and update the nginx

    kubectl patch -n kube-system deployment metrics-server --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' solve the metrics-server not start and then builder exit because of metrices-listener failed 
  solution from this article https://gist.github.com/sanketsudake/a089e691286bf2189bfedf295222bd43
    Kind cluster host : https://127.0.0.1:52818/
# 02-01
    setup yatai-builder and deployer
    https://kind.sigs.k8s.io/docs/user/ingress/
    https://kubernetes.io/docs/concepts/services-networking/ingress-controllers/
    https://kubernetes.github.io/ingress-nginx/deploy/#quick-start
    https://app.slack.com/client/TK999HSJU/D06GGETEV36

# 01-31
    user mgt module in yatai
    https://github.com/bentoml/Yatai/issues/499